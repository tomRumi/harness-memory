// Package server exposes the memory over MCP (streamable HTTP, loopback) with nine tools.
package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tomRumi/harness-memory/internal/access"
	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/store"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// Quotas per layer for an unrestricted recall, and the cap on one hit's text.
const (
	QuotaWiki   = 2
	QuotaMemory = 1
	QuotaGlobal = 1
	HitChars    = 600
	Version     = "0.1.0"
	// MaxQuery is the longest recall query, in characters; a longer one is cut.
	MaxQuery = 2000
)

// Server holds what the tools need. The vector matrix is loaded once and reloaded after a write.
type Server struct {
	St  *store.Store
	DB  *index.DB
	Emb index.Embedder
	// TokensDir, if set, turns token mode on: /mcp needs a bearer token listed in a file there (see package access).
	TokensDir string

	mu  sync.RWMutex
	mat *index.Matrix
}

// NewReader makes a server that can only answer recall: no store, no writes. The command-line context format uses it.
func NewReader(db *index.DB, emb index.Embedder) (*Server, error) {
	s := &Server{DB: db, Emb: emb}
	return s, s.Reload()
}

func New(st *store.Store, db *index.DB, emb index.Embedder) (*Server, error) {
	s := &Server{St: st, DB: db, Emb: emb}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	st.OnWrite = func() { s.Reload() }
	st.Matrix = s.matrix
	return s, nil
}

// Reload reads the vectors again (after a rescan or a write).
func (s *Server) Reload() error {
	if s.Emb == nil {
		return nil
	}
	m, err := s.DB.LoadMatrix(s.Emb.ID())
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.mat = m
	s.mu.Unlock()
	return nil
}

func (s *Server) matrix() *index.Matrix {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mat
}

func text(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
}

// ---- recall

type RecallIn struct {
	Query   string `json:"query" jsonschema:"what to look for, in plain words"`
	Project string `json:"project,omitempty" jsonschema:"project name; empty searches every project"`
	Layer   string `json:"layer,omitempty" jsonschema:"auto (default), wiki, memory or fact"`
	Budget  int    `json:"budget,omitempty" jsonschema:"maximum characters in the answer (0 means no limit)"`

	noGlobal bool // leave out the pass over the global project (a token that is not granted it)
}

func label(h index.Hit) string {
	l := h.Path
	if h.Section != "" {
		l += "::" + h.Section
	}
	if h.Kind == "note" || h.Kind == "fact" {
		l += " #" + store.ShortID(h.NodeID)
	}
	if h.Status == store.Unverified { // a note nobody has checked yet says so, and by whom it was written
		l += " " + h.Status
		if h.Author != "" {
			l += ", by " + h.Author
		}
	}
	return l
}

func clip(t string, n int) string { return wiki.Clip(strings.Join(strings.Fields(t), " "), n) }

// Recall answers a query. It never returns an error to the caller: an empty index, no result and a failing part are
// three different lines (`not built`, `no results`, `unavailable: <reason>`).
func (s *Server) Recall(ctx context.Context, in RecallIn) string {
	lines, warn, early := s.recall(ctx, in)
	if early != "" {
		return early
	}
	if warn != nil {
		lines = append([]string{VectorsHeader(warn)}, lines...)
	}
	return budgeted(strings.Join(lines, "\n"), in.Budget)
}

// VectorsHeader is the one line that goes first in an answer that had to use keywords only.
func VectorsHeader(reason error) string {
	r := strings.TrimSpace(strings.SplitN(reason.Error(), "\n", 2)[0])
	return fmt.Sprintf("[vectors unavailable: %s; keyword match only]", r)
}

func budgeted(out string, budget int) string {
	if budget > 0 && len([]rune(out)) > budget {
		return string([]rune(out)[:budget]) + "…"
	}
	return out
}

// recall builds the lines of an answer. warn is why the vector pass could not be used (nil when it was, or when there
// is no embedder); it is reported once, not per layer. early is a complete answer that needs no more work.
func (s *Server) recall(ctx context.Context, in RecallIn) (lines []string, warn error, early string) {
	q := strings.TrimSpace(in.Query)
	if r := []rune(q); len(r) > MaxQuery {
		q = string(r[:MaxQuery]) // cut, not an error: a long query still finds what it is about
	}
	if q == "" {
		return nil, nil, "[recall: empty query]"
	}
	layer := strings.ToLower(strings.TrimSpace(in.Layer))
	type pass struct {
		tag   string
		kinds []string
		quota int
	}
	all := []pass{{"wiki", []string{"wiki"}, QuotaWiki}, {"memory", []string{"note", "fact"}, QuotaMemory}}
	var passes []pass
	switch layer {
	case "", "auto":
		passes = all
	case "wiki":
		passes = []pass{{"wiki", []string{"wiki"}, 5}}
	case "memory", "note", "notes":
		passes = []pass{{"memory", []string{"note", "fact"}, 5}}
	case "fact", "facts":
		passes = []pass{{"fact", []string{"fact"}, 5}}
	default:
		return nil, nil, fmt.Sprintf("[recall: unknown layer %q — use auto, wiki, memory or fact]", in.Layer)
	}
	proj, err := wiki.OptionalProject(in.Project)
	if err != nil {
		return nil, nil, "[recall: " + err.Error() + "]"
	}
	emb := s.Emb // dropped after the first failure: the other layers do not wait for a dead embedder again
	mat := s.matrix()
	if emb != nil {
		warn = index.ProbeIfEmpty(ctx, emb, mat)
		if warn != nil {
			emb = nil
		}
	}
	run := func(tag string, kinds []string, project string, quota int, prefix string) {
		var n int
		args := []any{}
		sq := `SELECT count(*) FROM nodes WHERE kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",") + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
		if project != "" {
			sq += ` AND project = ?`
			args = append(args, project)
		}
		if err := s.DB.QueryRow(sq, args...).Scan(&n); err != nil {
			lines = append(lines, fmt.Sprintf("[%s%s unavailable: %v]", prefix, tag, err))
			return
		}
		if n == 0 {
			where := project
			if where == "" {
				where = "any project"
			}
			lines = append(lines, fmt.Sprintf("[%s%s: not built for %s]", prefix, tag, where))
			return
		}
		f := index.Filter{Project: project, Kinds: kinds, Limit: quota}
		hits, via, w := s.DB.Hybrid(ctx, emb, mat, q, f)
		if via == "" && w != nil { // the keyword search itself failed
			lines = append(lines, fmt.Sprintf("[%s%s unavailable: %v]", prefix, tag, w))
			return
		}
		if w != nil {
			if warn == nil {
				warn = w
			}
			emb = nil
		}
		if len(hits) == 0 {
			lines = append(lines, fmt.Sprintf("[%s%s: no results]", prefix, tag))
			return
		}
		for _, h := range hits {
			lines = append(lines, fmt.Sprintf("[%s%s %s] %s", prefix, tag, label(h), clip(h.Text, HitChars)))
		}
	}
	for _, p := range passes {
		run(p.tag, p.kinds, proj, p.quota, "")
	}
	if !in.noGlobal && proj != "" && proj != store.GlobalProject && (layer == "" || layer == "auto" || layer == "memory") {
		var n int
		s.DB.QueryRow(`SELECT count(*) FROM nodes WHERE kind IN ('note','fact') AND project = ?`, store.GlobalProject).Scan(&n)
		if n > 0 {
			run("memory", []string{"note", "fact"}, store.GlobalProject, QuotaGlobal, "global ")
		}
	}
	if s.St != nil {
		s.St.Event("recall", "", proj, clip(q, 100))
	}
	return lines, warn, ""
}

// ---- stats

type StatsIn struct {
	Project string `json:"project,omitempty" jsonschema:"project name; empty counts everything"`
}

// Stats is also the connection health check: it reports the state of every part the other tools depend on.
func (s *Server) Stats(ctx context.Context, in StatsIn) string {
	proj, err := wiki.OptionalProject(in.Project)
	if err != nil {
		return "[stats: " + err.Error() + "]"
	}
	st, err := s.DB.Stats()
	if err != nil {
		return "[stats unavailable: " + err.Error() + "]"
	}
	var keys []string
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "harness-memory-service %s\n", Version)
	total := 0
	for _, k := range keys {
		if proj != "" && !strings.HasSuffix(k, "/"+proj) {
			continue
		}
		fmt.Fprintf(&b, "%-36s %d\n", k, st[k])
		total += st[k]
	}
	if total == 0 {
		fmt.Fprintf(&b, "[not built for %q]\n", proj)
	}
	var notes int
	s.DB.QueryRow(`SELECT count(*) FROM nodes WHERE kind='note'`+projClause(proj), projArgs(proj)...).Scan(&notes)
	fmt.Fprintf(&b, "notes waiting: %d\n", notes)
	b.WriteString(VectorLine(ctx, s.DB, s.Emb) + "\n")
	if s.St != nil && s.Emb != nil && s.St.EmbedError() != nil {
		fmt.Fprintf(&b, "vectors: the last write could not be embedded (%v); the catch-up or `hms embed` will do it\n", s.St.EmbedError())
	}
	if s.St != nil && s.St.Commit != nil {
		c := s.St.Commit
		commits, lastErr, _, enabled := c.Status()
		switch {
		case !enabled:
			b.WriteString("commits: off, the memory directory is not a git repository (notes are saved as files only)\n")
		case lastErr != nil:
			fmt.Fprintf(&b, "commits: last commit failed (retrying): %v\n", lastErr)
		case c.Busy():
			b.WriteString("commits: paused, the repository is in the middle of a merge or rebase\n")
		case c.Detached():
			b.WriteString("commits: paused, HEAD is detached; check out a branch\n")
		default:
			fmt.Fprintf(&b, "commits: ok (%d made since start)\n", commits)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// VectorLine is the vector state for stats: on with the model and the number of embedded chunks, off, or unavailable
// with the reason. It asks the embedder (at most 10 s); with no embedder it makes no call.
func VectorLine(ctx context.Context, db *index.DB, emb index.Embedder) string {
	if emb == nil {
		return "vectors: off"
	}
	hc, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := emb.Query(hc, "health"); err != nil {
		return fmt.Sprintf("vectors: unavailable: %s (keyword match only)", strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0]))
	}
	n, total, err := db.ChunkCounts(emb.ID())
	if err != nil {
		return fmt.Sprintf("vectors: on (%s), chunk count unavailable: %v", emb.Model(), err)
	}
	return fmt.Sprintf("vectors: on (%s), %d of %d chunks embedded", emb.Model(), n, total)
}

// CatchUp embeds the chunks that have no vector, once now and then every interval, until ctx ends. It runs beside the
// service and never holds the write lock: a write or a recall does not wait for it. A failing embedder is not an
// error here; the next round tries again. After a round that embedded something the vector matrix is reloaded.
func (s *Server) CatchUp(ctx context.Context, interval time.Duration) {
	if s.Emb == nil {
		return
	}
	round := func() {
		missing, _, err := s.DB.Pending(s.Emb.ID())
		if err != nil || missing == 0 {
			return
		}
		if n, _ := s.DB.EmbedPending(ctx, s.Emb, nil); n > 0 {
			s.Reload()
		}
	}
	round()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			round()
		}
	}
}

func projClause(p string) string {
	if p == "" {
		return ""
	}
	return " AND project = ?"
}
func projArgs(p string) []any {
	if p == "" {
		return nil
	}
	return []any{p}
}

// ---- the other tools

type LearnIn struct {
	Text       string `json:"text" jsonschema:"one learning, a sentence or two (cut at 500 characters)"`
	Project    string `json:"project,omitempty" jsonschema:"project name; empty means global"`
	Kind       string `json:"kind,omitempty" jsonschema:"optional short label such as topic:type, for example rag:gotcha (default learning); informational only, it does not decide where the note is merged"`
	Supersedes string `json:"supersedes,omitempty" jsonschema:"short id (or unique prefix) of the note this one replaces"`
}
type FactIn struct {
	Text     string  `json:"text" jsonschema:"one durable fact"`
	Project  string  `json:"project,omitempty" jsonschema:"project name; empty means global"`
	Category string  `json:"category,omitempty" jsonschema:"for example user_pref, project, tool, general"`
	Trust    float64 `json:"trust,omitempty" jsonschema:"0 to 1, default 0.5"`
}
type VerifyIn struct {
	ID      string `json:"id" jsonschema:"short id recall prints, or a unique prefix of it (at least 4 characters)"`
	Project string `json:"project,omitempty" jsonschema:"restrict the lookup to this project"`
}
type ListIn struct {
	Project string `json:"project,omitempty" jsonschema:"the project whose notes to list; required without tokens"`
	Status  string `json:"status,omitempty" jsonschema:"empty for all, unverified or active"`
	Limit   int    `json:"limit,omitempty" jsonschema:"notes per page, default 50, at most 200"`
	Offset  int    `json:"offset,omitempty" jsonschema:"notes to skip, default 0"`
}
type WikiReadIn struct {
	Path string `json:"path" jsonschema:"path under wiki/ (project/Page Title.md) or a page title (Page Title, or project/Page Title)"`
}
type RetireIn struct {
	ID      string `json:"id" jsonschema:"short id recall prints, or a unique prefix of it (at least 4 characters)"`
	Reason  string `json:"reason,omitempty" jsonschema:"why the note is withdrawn"`
	Project string `json:"project,omitempty" jsonschema:"restrict the lookup to this project"`
}
type FeedbackIn struct {
	ID      string `json:"id" jsonschema:"short id recall printed"`
	Rating  string `json:"rating" jsonschema:"helpful or unhelpful"`
	Session string `json:"session" jsonschema:"required: a stable name for this conversation or run; only a session's last rating counts"`
	Project string `json:"project,omitempty" jsonschema:"restrict the lookup to this project"`
}

// Rights returns the rights of the token of a call: nil in open mode, so that a caller can ask "is there a token" by
// testing for nil. It is the accessor for the rights of the current call. In token mode a call without rights is an error, never open access.
func (s *Server) Rights(req *mcp.CallToolRequest) (*access.Rights, error) {
	if s.TokensDir == "" {
		return nil, nil
	}
	if req != nil && req.Extra != nil {
		if r := access.Of(req.Extra.TokenInfo); r != nil {
			return r, nil
		}
	}
	return nil, fmt.Errorf("this call carries no token rights; nothing was done")
}

// scope turns the project of a call into the project to use. In open mode it is the project as given (and the rights
// are nil). In token mode it is the one the token may use (see access.Rights.Resolve), and write says the call
// changes the memory, which needs the write right in that project.
func (s *Server) scope(req *mcp.CallToolRequest, project string, write bool) (string, *access.Rights, error) {
	r, err := s.Rights(req)
	if r == nil || err != nil {
		return project, nil, err
	}
	p, err := r.Resolve(project)
	if err != nil {
		return "", r, err
	}
	if write && !r.Grants[p].Write {
		return "", r, fmt.Errorf("this token may not write to project %s; nothing was done", p)
	}
	return p, r, nil
}

// writer is who a write is made by: nil in open mode, else the name of the grant (or the start of the token digest) and
// whether the grant is staged, which makes its notes wait for a check.
func writer(r *access.Rights, project string) *store.Writer {
	if r == nil {
		return nil
	}
	g := r.Grants[project]
	w := &store.Writer{Author: g.Name, Staged: g.LearnStatus == store.Unverified}
	if w.Author == "" {
		w.Author = r.Digest[:8]
	}
	return w
}

// Handler builds the MCP HTTP handler. Each connection gets its own MCP server object over the shared state.
func (s *Server) Handler() http.Handler {
	build := func(*http.Request) *mcp.Server {
		m := mcp.NewServer(&mcp.Implementation{Name: "harness-memory-service", Version: Version}, nil)
		mcp.AddTool(m, &mcp.Tool{Name: "recall", Description: "Search the memory: wiki entries, notes and facts. Returns short hits with a path and, for notes, a #id to use with retire, feedback, verify or learn(supersedes). A note marked unverified has not been checked yet."},
			func(ctx context.Context, req *mcp.CallToolRequest, in RecallIn) (*mcp.CallToolResult, any, error) {
				proj, r, err := s.scope(req, in.Project, false)
				if err != nil {
					return nil, nil, err
				}
				in.Project, in.noGlobal = proj, r != nil && !r.Allows(store.GlobalProject)
				return text(s.Recall(ctx, in))
			})
		mcp.AddTool(m, &mcp.Tool{Name: "learn", Description: "Store one learning (not routine actions). Idempotent: the same text returns the same id. A staged token's note is stored unverified until someone verifies it. kind is an optional short label such as topic:type; it is informational only and does not decide where the note is merged."},
			func(ctx context.Context, req *mcp.CallToolRequest, in LearnIn) (*mcp.CallToolResult, any, error) {
				proj, r, err := s.scope(req, in.Project, true)
				if err != nil {
					return nil, nil, err
				}
				id, err := s.St.LearnAs(ctx, in.Text, proj, in.Kind, in.Supersedes, writer(r, proj))
				if err != nil {
					return nil, nil, err
				}
				return text(id)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "wiki_read", Description: "Read one wiki page in full by its path under wiki/ or by its title. The answer ends with the pages it links to, the pages that link to it and the pages related to it by meaning."},
			func(ctx context.Context, req *mcp.CallToolRequest, in WikiReadIn) (*mcp.CallToolResult, any, error) {
				r, err := s.Rights(req)
				if err != nil {
					return nil, nil, err
				}
				var allow func(string) bool
				if r != nil {
					allow = r.Allows
				}
				t, err := s.St.WikiReadFor(in.Path, allow)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "fact_add", Description: "Store one durable fact (preference, decision, environment)."},
			func(ctx context.Context, req *mcp.CallToolRequest, in FactIn) (*mcp.CallToolResult, any, error) {
				proj, r, err := s.scope(req, in.Project, true)
				if err != nil {
					return nil, nil, err
				}
				if w := writer(r, proj); w != nil && w.Staged {
					return nil, nil, fmt.Errorf("this token is staged: its writes wait for a check and facts have none, so it may not add facts; use learn. Nothing was done")
				}
				id, err := s.St.AddFact(ctx, in.Text, proj, in.Category, in.Trust)
				if err != nil {
					return nil, nil, err
				}
				return text(id)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "retire", Description: "Delete a note or fact that is wrong or no longer applies. The file is removed after its text is in a git commit; if that cannot be done nothing is deleted and the answer says why. An unknown or ambiguous id changes nothing."},
			func(ctx context.Context, req *mcp.CallToolRequest, in RetireIn) (*mcp.CallToolResult, any, error) {
				proj, r, err := s.scope(req, in.Project, true)
				if err != nil {
					return nil, nil, err
				}
				w := writer(r, proj)
				t, err := s.St.RetireAs(ctx, in.ID, in.Reason, proj, w != nil && w.Staged)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "verify", Description: "Mark an unverified note as checked: it then counts as an ordinary note. Needs the verify right. An unknown or ambiguous id, or a note that is already active, changes nothing."},
			func(ctx context.Context, req *mcp.CallToolRequest, in VerifyIn) (*mcp.CallToolResult, any, error) {
				proj, r, err := s.scope(req, in.Project, false)
				if err != nil {
					return nil, nil, err
				}
				if r != nil && !r.Grants[proj].Verify {
					return nil, nil, fmt.Errorf("this token may not verify notes in project %s; nothing was done", proj)
				}
				t, err := s.St.Verify(ctx, in.ID, proj)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "list_memory", Description: "List the notes of one project, newest first, as JSON: id, short id, kind, status (unverified or active), author, created and the full text, with the total that matches. Use it to find the unverified notes; recall is ranked and cannot list."},
			func(ctx context.Context, req *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, any, error) {
				if r, err := s.Rights(req); err == nil && r == nil && strings.TrimSpace(in.Project) == "" {
					return nil, nil, fmt.Errorf("give the project whose notes to list; nothing was listed")
				}
				proj, _, err := s.scope(req, in.Project, false)
				if err != nil {
					return nil, nil, err
				}
				t, err := s.St.ListNotes(proj, in.Status, in.Limit, in.Offset)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "feedback", Description: "Say whether a recalled note helped (helpful or unhelpful). A session name is required; each session counts once."},
			func(ctx context.Context, req *mcp.CallToolRequest, in FeedbackIn) (*mcp.CallToolResult, any, error) {
				proj, _, err := s.scope(req, in.Project, false)
				if err != nil {
					return nil, nil, err
				}
				r := strings.ToLower(strings.TrimSpace(in.Rating))
				if r != "helpful" && r != "unhelpful" {
					return text(fmt.Sprintf("rating must be helpful or unhelpful, not %q — nothing recorded", in.Rating))
				}
				t, err := s.St.Feedback(in.ID, r == "helpful", proj, in.Session)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "stats", Description: "Counts per layer, embedder and commit health. Use it as the connection check."},
			func(ctx context.Context, req *mcp.CallToolRequest, in StatsIn) (*mcp.CallToolResult, any, error) {
				proj, _, err := s.scope(req, in.Project, false)
				if err != nil {
					return nil, nil, err
				}
				in.Project = proj
				return text(s.Stats(ctx, in))
			})
		return m
	}
	mux := http.NewServeMux()
	var mcpHandler http.Handler = mcp.NewStreamableHTTPHandler(build, &mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute})
	if s.TokensDir != "" { // token mode: the SDK's own middleware, a token file check on every request
		mcpHandler = auth.RequireBearerToken(access.Verifier(s.TokensDir), &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(mcpHandler)
	}
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) { // the same check as /mcp: a page in a browser must not read the stats of this machine
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if s.TokensDir != "" { // no token is asked here, so no project may be named
			fmt.Fprintln(w, "ok")
			return
		}
		fmt.Fprintln(w, s.Stats(r.Context(), StatsIn{}))
	})
	return mux
}

// loopbackHost says whether a Host header names this machine: localhost, 127.x.x.x or ::1, with or without a port.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var noiseLine = regexp.MustCompile(`^\[(global )?(wiki|memory|fact)(:| unavailable)`)

// ContextBlock is the text a harness hook adds in front of a prompt: only real hits, no empty-state lines (a prompt
// that finds nothing adds nothing). It is empty when there is nothing to add. warn is why vectors were not used, for
// the caller to put on standard error; it is never part of the block.
func (s *Server) ContextBlock(ctx context.Context, query, project string, budget int) (block string, warn error) {
	all, warn, early := s.recall(ctx, RecallIn{Query: query, Project: project})
	if early != "" {
		return "", nil
	}
	var keep []string
	for _, l := range strings.Split(budgeted(strings.Join(all, "\n"), budget), "\n") {
		if strings.TrimSpace(l) != "" && !noiseLine.MatchString(l) {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		return "", warn
	}
	return "<memory-context source=\"harness-memory-service\" note=\"recalled automatically from earlier work; check it before relying on it\">\n" + strings.Join(keep, "\n") + "\n</memory-context>", warn
}
