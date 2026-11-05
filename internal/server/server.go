// Package server exposes the memory over MCP (streamable HTTP, loopback) with seven tools.
package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

// Quotas per layer for an unrestricted recall, and the cap on one hit's text.
const (
	QuotaWiki   = 2
	QuotaMemory = 1
	QuotaGlobal = 1
	HitChars    = 600
	Version     = "0.1.0"
)

// Server holds what the tools need. The vector matrix is loaded once and reloaded after a write.
type Server struct {
	St  *store.Store
	DB  *index.DB
	Emb index.Embedder

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
}

func label(h index.Hit) string {
	l := h.Path
	if h.Section != "" {
		l += "::" + h.Section
	}
	if h.Kind == "note" || h.Kind == "fact" {
		id := h.NodeID[strings.LastIndex(h.NodeID, "/")+1:]
		if len(id) > 8 {
			id = id[:8]
		}
		l += " #" + id
	}
	return l
}

func clip(t string, n int) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > n {
		return string(r[:n]) + "…"
	}
	return t
}

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
	proj := strings.ToLower(strings.TrimSpace(in.Project))
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
	if proj != "" && proj != store.GlobalProject && (layer == "" || layer == "auto" || layer == "memory") {
		var n int
		s.DB.QueryRow(`SELECT count(*) FROM nodes WHERE kind IN ('note','fact') AND project = ?`, store.GlobalProject).Scan(&n)
		if n > 0 {
			var before = len(lines)
			run("memory", []string{"note", "fact"}, store.GlobalProject, QuotaGlobal, "global ")
			_ = before
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
	proj := strings.ToLower(strings.TrimSpace(in.Project))
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
	Kind       string `json:"kind" jsonschema:"module:topic, for example rag:gotcha; decides where the note is merged later"`
	Supersedes string `json:"supersedes,omitempty" jsonschema:"short id (or unique prefix) of the note this one replaces"`
}
type FactIn struct {
	Text     string  `json:"text" jsonschema:"one durable fact"`
	Project  string  `json:"project,omitempty" jsonschema:"project name; empty means global"`
	Category string  `json:"category,omitempty" jsonschema:"for example user_pref, project, tool, general"`
	Trust    float64 `json:"trust,omitempty" jsonschema:"0 to 1, default 0.5"`
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

// Handler builds the MCP HTTP handler. Each connection gets its own MCP server object over the shared state.
func (s *Server) Handler() http.Handler {
	build := func(*http.Request) *mcp.Server {
		m := mcp.NewServer(&mcp.Implementation{Name: "harness-memory-service", Version: Version}, nil)
		mcp.AddTool(m, &mcp.Tool{Name: "recall", Description: "Search the memory: wiki entries, notes and facts. Returns short hits with a path and, for notes, a #id to use with retire, feedback or learn(supersedes)."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in RecallIn) (*mcp.CallToolResult, any, error) {
				return text(s.Recall(ctx, in))
			})
		mcp.AddTool(m, &mcp.Tool{Name: "learn", Description: "Store one learning (not routine actions). Idempotent: the same text returns the same id."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in LearnIn) (*mcp.CallToolResult, any, error) {
				id, err := s.St.Learn(ctx, in.Text, in.Project, in.Kind, in.Supersedes)
				if err != nil {
					return nil, nil, err
				}
				return text(id)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "wiki_read", Description: "Read one wiki page in full by its path under wiki/ or by its title. The answer ends with the pages it links to, the pages that link to it and the pages related to it by meaning."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in WikiReadIn) (*mcp.CallToolResult, any, error) {
				t, err := s.St.WikiRead(in.Path)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "fact_add", Description: "Store one durable fact (preference, decision, environment)."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in FactIn) (*mcp.CallToolResult, any, error) {
				id, err := s.St.AddFact(ctx, in.Text, in.Project, in.Category, in.Trust)
				if err != nil {
					return nil, nil, err
				}
				return text(id)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "retire", Description: "Delete a note or fact that is wrong or no longer applies. The file is removed and git history keeps its text. An unknown or ambiguous id changes nothing."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in RetireIn) (*mcp.CallToolResult, any, error) {
				t, err := s.St.Retire(ctx, in.ID, in.Reason, in.Project)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "feedback", Description: "Say whether a recalled note helped (helpful or unhelpful). A session name is required; each session counts once."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in FeedbackIn) (*mcp.CallToolResult, any, error) {
				r := strings.ToLower(strings.TrimSpace(in.Rating))
				if r != "helpful" && r != "unhelpful" {
					return text(fmt.Sprintf("rating must be helpful or unhelpful, not %q — nothing recorded", in.Rating))
				}
				t, err := s.St.Feedback(in.ID, r == "helpful", in.Project, in.Session)
				if err != nil {
					return nil, nil, err
				}
				return text(t)
			})
		mcp.AddTool(m, &mcp.Tool{Name: "stats", Description: "Counts per layer, embedder and commit health. Use it as the connection check."},
			func(ctx context.Context, _ *mcp.CallToolRequest, in StatsIn) (*mcp.CallToolResult, any, error) {
				return text(s.Stats(ctx, in))
			})
		return m
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(build, &mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, s.Stats(r.Context(), StatsIn{}))
	})
	return mux
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
