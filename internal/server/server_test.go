package server

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/store"
)

type bag struct {
	mu   sync.Mutex
	down bool
}

func (b *bag) setDown(v bool) { b.mu.Lock(); b.down = v; b.mu.Unlock() }
func (b *bag) isDown() bool   { b.mu.Lock(); defer b.mu.Unlock(); return b.down }

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func (b *bag) vec(text string) []float32 {
	v := make([]float32, 32)
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%32]++
	}
	var s float64
	for _, x := range v {
		s += float64(x * x)
	}
	if s == 0 {
		v[0], s = 1, 1
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(s))
	}
	return v
}
func (b *bag) Model() string { return "bag" }
func (b *bag) ID() string    { return "bag" }
func (b *bag) Docs(ctx context.Context, t []string) ([][]float32, error) {
	if b.isDown() {
		return nil, fmt.Errorf("embedder down")
	}
	o := make([][]float32, len(t))
	for i, x := range t {
		o[i] = b.vec(x)
	}
	return o, nil
}
func (b *bag) Query(ctx context.Context, t string) ([]float32, error) {
	if b.isDown() {
		return nil, fmt.Errorf("embedder down")
	}
	return b.vec(t), nil
}

func setup(t *testing.T) (*mcp.ClientSession, *Server, string, *bag, string) {
	t.Helper()
	home := t.TempDir()
	repo := filepath.Join(home, "memory")
	for _, d := range []string{"wiki/proj", "notes", "facts"} {
		os.MkdirAll(filepath.Join(repo, d), 0o755)
	}
	os.WriteFile(filepath.Join(repo, "wiki", "proj", "ports.md"), []byte("# Ports\n\n## Daemon\nThe daemon listens on port 8765 over loopback only.\n"), 0o644)
	exec.Command("git", "-C", repo, "init", "-q", "-b", "main").Run()
	exec.Command("git", "-C", repo, "config", "user.email", "t@example.invalid").Run()
	exec.Command("git", "-C", repo, "config", "user.name", "t").Run()
	db, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	e := &bag{}
	db.EmbedPending(context.Background(), e, nil)
	st, err := store.New(repo, db, e, store.NewCommitter(repo, 40*time.Millisecond), filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(st, db, e)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, srv, repo, e, ts.URL
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestEightToolsAndTheirSchemaSize(t *testing.T) {
	s, _, _, _, _ := setup(t)
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	raw, _ := json.Marshal(tools.Tools)
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	want := "fact_add feedback learn recall retire stats verify wiki_read"
	got := strings.Join(sortStrings(names), " ")
	if got != want {
		t.Fatalf("tools %q, want %q", got, want)
	}
	t.Logf("tools/list: %d tools, %d bytes of JSON, about %d tokens at 4 bytes per token", len(tools.Tools), len(raw), len(raw)/4)
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func TestRecallThreeEmptyStatesAndAHit(t *testing.T) {
	s, _, _, _, _ := setup(t)
	out, _ := call(t, s, "recall", map[string]any{"query": "daemon port", "project": "proj"})
	t.Logf("recall output:\n%s", out)
	if !strings.Contains(out, "[wiki wiki/proj/ports.md::Daemon] ") || !strings.Contains(out, "8765") {
		t.Fatalf("no wiki hit:\n%s", out)
	}
	if !strings.Contains(out, "[memory: not built for proj]") {
		t.Fatalf("empty memory layer must say 'not built':\n%s", out)
	}
	out, _ = call(t, s, "recall", map[string]any{"query": "zxqv unmatched", "project": "proj", "layer": "wiki"})
	if !strings.Contains(out, "[wiki: no results]") && !strings.Contains(out, "[wiki wiki/") {
		t.Fatalf("%s", out)
	}
	if out, _ = call(t, s, "recall", map[string]any{"query": "x", "layer": "bogus"}); !strings.Contains(out, "unknown layer") {
		t.Fatalf("%s", out)
	}
	if out, _ = call(t, s, "recall", map[string]any{"query": "  "}); !strings.Contains(out, "empty query") {
		t.Fatalf("%s", out)
	}
}

func TestEmbedderDownIsAnUnavailableLineNotAnError(t *testing.T) {
	s, srv, _, e, _ := setup(t)
	e.setDown(true)
	out, isErr := call(t, s, "recall", map[string]any{"query": "daemon port", "project": "proj", "layer": "wiki"})
	if isErr || !strings.Contains(out, "keyword match only") || !strings.Contains(out, "8765") {
		t.Fatalf("recall must fall back to keywords and say so: %v\n%s", isErr, out)
	}
	st := srv.Stats(context.Background(), StatsIn{})
	if !strings.Contains(st, "vectors: unavailable") {
		t.Fatalf("stats must report the embedder state:\n%s", st)
	}
	if id, isErr := call(t, s, "learn", map[string]any{"text": "A note about the zorblax cache written while down.", "project": "proj", "kind": "k"}); isErr || len(id) < 36 {
		t.Fatalf("learn must succeed while the embedder is down: %q %v", id, isErr)
	}
	out, _ = call(t, s, "recall", map[string]any{"query": "zorblax cache", "project": "proj", "layer": "memory"})
	if !strings.Contains(out, "zorblax") {
		t.Fatalf("a note written while the embedder was down must be found by keyword:\n%s", out)
	}
	if st := srv.Stats(context.Background(), StatsIn{}); !strings.Contains(st, "vectors: unavailable") {
		t.Fatalf("stats must still report the embedder as unavailable:\n%s", st)
	}
}

func TestRecallLayersAreWikiMemoryAndFactOnly(t *testing.T) {
	s, _, _, _, _ := setup(t)
	call(t, s, "learn", map[string]any{"text": "A note about the quuxlet scheduler.", "project": "proj", "kind": "k"})
	call(t, s, "fact_add", map[string]any{"text": "The quuxlet scheduler is owned by the platform team.", "project": "proj"})
	out, _ := call(t, s, "recall", map[string]any{"query": "quuxlet scheduler", "project": "proj"})
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "[wiki") && !strings.HasPrefix(l, "[memory") && !strings.HasPrefix(l, "[global memory") {
			t.Fatalf("auto recall gave a line outside wiki and memory: %q\n%s", l, out)
		}
	}
	if !strings.Contains(out, "[memory ") {
		t.Fatalf("no memory hit:\n%s", out)
	}
	if out, _ = call(t, s, "recall", map[string]any{"query": "quuxlet scheduler", "project": "proj", "layer": "fact"}); !strings.Contains(out, "[fact ") || strings.Contains(out, "[memory") {
		t.Fatalf("fact layer:\n%s", out)
	}
	if out, _ = call(t, s, "recall", map[string]any{"query": "quuxlet", "layer": "memory"}); !strings.Contains(out, "[memory ") {
		t.Fatalf("memory layer:\n%s", out)
	}
	if out, _ = call(t, s, "recall", map[string]any{"query": "quuxlet", "layer": "code"}); !strings.Contains(out, "unknown layer") || strings.Contains(out, "code,") {
		t.Fatalf("code must be an unknown layer:\n%s", out)
	}
}

func TestLearnRecallRetireRoundTrip(t *testing.T) {
	s, _, repo, _, _ := setup(t)
	id, isErr := call(t, s, "learn", map[string]any{"text": "The weekly job runs on Sunday at 03:00 and needs the vpn.", "project": "proj", "kind": "ops:schedule"})
	if isErr || len(id) < 36 {
		t.Fatalf("learn: %q %v", id, isErr)
	}
	out, _ := call(t, s, "recall", map[string]any{"query": "weekly job sunday", "project": "proj", "layer": "memory"})
	short := id[:8]
	if !strings.Contains(out, "#"+short) || !strings.Contains(out, "vpn") {
		t.Fatalf("learned note not recalled with its id:\n%s", out)
	}
	if out, _ := call(t, s, "feedback", map[string]any{"id": short, "rating": "helpful", "session": "s1", "project": "proj"}); !strings.Contains(out, "helpful 1") {
		t.Fatalf("%s", out)
	}
	if out, _ := call(t, s, "feedback", map[string]any{"id": short, "rating": "maybe", "session": "s1"}); !strings.Contains(out, "nothing recorded") {
		t.Fatalf("%s", out)
	}
	if out, _ := call(t, s, "retire", map[string]any{"id": short, "reason": "obsolete", "project": "proj"}); !strings.HasPrefix(out, "deleted "+short) {
		t.Fatalf("%s", out)
	}
	out, _ = call(t, s, "recall", map[string]any{"query": "weekly job sunday", "project": "proj", "layer": "memory"})
	if strings.Contains(out, "#"+short+"]") {
		t.Fatalf("retired note is still recalled:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(repo, "notes", "proj", id+".md")); !os.IsNotExist(err) {
		t.Fatalf("the retired note's file must be gone: %v", err)
	}
	fact, _ := call(t, s, "fact_add", map[string]any{"text": "The owner prefers short answers.", "category": "user_pref"})
	if len(fact) < 36 {
		t.Fatalf("fact_add: %q", fact)
	}
	if _, err := os.Stat(filepath.Join(repo, "facts", "global", fact+".md")); err != nil {
		t.Fatal(err)
	}
	wiki, isErr := call(t, s, "wiki_read", map[string]any{"path": "proj/ports.md"})
	if isErr || !strings.Contains(wiki, "port 8765") {
		t.Fatalf("%q %v", wiki, isErr)
	}
	if _, isErr := call(t, s, "wiki_read", map[string]any{"path": "../notes/x.md"}); !isErr {
		t.Fatal("path traversal must fail")
	}
}

func TestStatsAndHealthz(t *testing.T) {
	_, srv, _, _, url := setup(t)
	out := srv.Stats(context.Background(), StatsIn{})
	t.Logf("stats:\n%s", out)
	for _, want := range []string{"harness-memory-service", "wiki/proj", "vectors: on (bag)", "commits: ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats lacks %q:\n%s", want, out)
		}
	}
	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "vectors: on (bag)") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestContextBlockKeepsOnlyRealHits(t *testing.T) {
	_, full, _, _, _ := setup(t)
	srv, _ := NewReader(full.DB, nil) // keyword only, as the hook runs by default
	if got, _ := srv.ContextBlock(context.Background(), "zzzqqq nothing matches this", "", 1000); got != "" {
		t.Fatalf("a query without a hit must give an empty block, got %q", got)
	}
	got, _ := srv.ContextBlock(context.Background(), "daemon port loopback", "", 1000)
	if !strings.HasPrefix(got, "<memory-context") || !strings.HasSuffix(got, "</memory-context>") || strings.Contains(got, "no results") || strings.Contains(got, "not built") {
		t.Fatalf("%q", got)
	}
}

func countHeader(out string) int { return strings.Count(out, "[vectors unavailable:") }

func TestVectorsHeaderOnceAndFirstWithEmbedderDown(t *testing.T) {
	s, _, _, e, _ := setup(t)
	call(t, s, "learn", map[string]any{"text": "The daemon restarts itself after a crash.", "project": "proj", "kind": "k"})
	e.setDown(true)
	out, _ := call(t, s, "recall", map[string]any{"query": "daemon", "project": "proj"})
	if countHeader(out) != 1 || !strings.HasPrefix(out, "[vectors unavailable: embedder down; keyword match only]\n") {
		t.Fatalf("one header line first, got:\n%s", out)
	}
	if strings.Contains(out, "vectors unavailable (") {
		t.Fatalf("no per-layer warning any more:\n%s", out)
	}
	e.setDown(false)
	out, _ = call(t, s, "recall", map[string]any{"query": "daemon", "project": "proj"})
	if countHeader(out) != 0 {
		t.Fatalf("no header while the embedder is up:\n%s", out)
	}
}

func TestVectorsHeaderWhenNothingIsEmbeddedYet(t *testing.T) {
	_, full, _, e, _ := setup(t)
	e.setDown(true)
	empty := &Server{DB: full.DB, Emb: e} // no matrix: Hybrid would never call the embedder
	out := empty.Recall(context.Background(), RecallIn{Query: "daemon port"})
	if countHeader(out) != 1 || !strings.Contains(out, "8765") {
		t.Fatalf("%s", out)
	}
}

func TestHookBlockNeverCarriesTheWarning(t *testing.T) {
	_, full, _, e, _ := setup(t)
	e.setDown(true)
	srv, _ := NewReader(full.DB, e)
	block, warn := srv.ContextBlock(context.Background(), "daemon port loopback", "", 1000)
	if warn == nil || strings.Contains(block, "vectors unavailable") || !strings.Contains(block, "8765") {
		t.Fatalf("warn %v block %q", warn, block)
	}
	if block, _ = srv.ContextBlock(context.Background(), "zzzqqq nothing matches", "", 1000); block != "" {
		t.Fatalf("nothing matching must give an empty block: %q", block)
	}
}

func TestStatsVectorLines(t *testing.T) {
	_, srv, _, e, _ := setup(t)
	ctx := context.Background()
	if st := srv.Stats(ctx, StatsIn{}); !strings.Contains(st, "vectors: on (bag), 2 of 2 chunks embedded") {
		t.Fatalf("%s", st)
	}
	e.setDown(true)
	if st := srv.Stats(ctx, StatsIn{}); !strings.Contains(st, "vectors: unavailable: embedder down (keyword match only)") {
		t.Fatalf("%s", st)
	}
	off := &Server{DB: srv.DB, St: srv.St}
	if st := off.Stats(ctx, StatsIn{}); !strings.Contains(st, "\nvectors: off\n") {
		t.Fatalf("%s", st)
	}
}

func TestCatchUpEmbedsPendingChunksAfterTheEmbedderComesUp(t *testing.T) {
	_, srv, _, e, _ := setup(t)
	e.setDown(true)
	if _, err := srv.St.Learn(context.Background(), "A note about the glimmerfox queue.", "proj", "k", ""); err != nil {
		t.Fatalf("a write must succeed while the embedder is down: %v", err)
	}
	if miss, _, _ := srv.DB.Pending("bag"); miss != 1 {
		t.Fatalf("one chunk must be waiting for a vector, got %d", miss)
	}
	before := len(srv.matrix().IDs)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.CatchUp(ctx, 20*time.Millisecond); close(done) }()
	time.Sleep(80 * time.Millisecond)
	if miss, _, _ := srv.DB.Pending("bag"); miss != 1 {
		t.Fatal("nothing can be embedded while the embedder is down")
	}
	e.setDown(false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if miss, _, _ := srv.DB.Pending("bag"); miss == 0 && len(srv.matrix().IDs) == before+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if miss, _, _ := srv.DB.Pending("bag"); miss != 0 || len(srv.matrix().IDs) != before+1 {
		t.Fatalf("catch-up must embed the chunk and reload the matrix: missing %d, matrix %d (was %d)", miss, len(srv.matrix().IDs), before)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop must stop when its context ends")
	}
}

func TestWikiReadByTitleAndRecallShowTitlesWithSpacesAndAccents(t *testing.T) {
	sess, srv, repo, _, _ := setup(t)
	rel := "wiki/proj/Őrzés és Café.md"
	os.WriteFile(filepath.Join(repo, rel), []byte("---\nsummary: Egy oldal\n---\n\n## Rész\nA quokkamarker szöveg, lásd [[ports]].\n"), 0o644)
	if err := srv.St.DB.Upsert(repo, rel); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, sess, "recall", map[string]any{"query": "quokkamarker", "project": "proj", "layer": "wiki"})
	if isErr || !strings.Contains(out, "[wiki wiki/proj/Őrzés és Café.md::Rész] ") {
		t.Fatalf("recall:\n%s", out)
	}
	for _, in := range []string{"proj/Őrzés és Café.md", "őrzés ÉS café", "proj/őrzés és café"} {
		got, isErr := call(t, sess, "wiki_read", map[string]any{"path": in})
		if isErr || !strings.Contains(got, "quokkamarker") || !strings.HasSuffix(got, "Links to: [[ports]]\n") {
			t.Fatalf("wiki_read(%q) = %q %v", in, got, isErr)
		}
	}
	got, _ := call(t, sess, "wiki_read", map[string]any{"path": "Ports"})
	if !strings.HasSuffix(got, "Linked from: [[Őrzés és Café]]\n") {
		t.Fatalf("backlink:\n%s", got)
	}
}

func TestWikiReadRelatedLineThroughTheServer(t *testing.T) {
	sess, srv, repo, e, _ := setup(t)
	rel := "wiki/proj/Ports again.md"
	os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte("# Ports\n\n## Daemon\nThe daemon listens on port 8765 over loopback only.\n"), 0o644)
	if err := srv.DB.Upsert(repo, rel); err != nil {
		t.Fatal(err)
	}
	if got, _ := call(t, sess, "wiki_read", map[string]any{"path": "proj/ports.md"}); strings.Contains(got, "Related:") {
		t.Fatalf("the new page has no vectors yet, so there is no line:\n%s", got)
	}
	srv.DB.EmbedPending(context.Background(), e, nil)
	srv.Reload()
	e.setDown(true) // stored vectors are used, the embedder is not called
	got, isErr := call(t, sess, "wiki_read", map[string]any{"path": "proj/ports.md"})
	if isErr || !strings.HasSuffix(got, "Related: [[Ports again]] (1.00)\n") {
		t.Fatalf("wiki_read:\n%s", got)
	}
}

func TestFreshNotesAreFoundNextToTheWikiHits(t *testing.T) {
	_, srv, _, _, _ := setup(t)
	ctx := context.Background()
	for _, txt := range []string{
		"The daemon port was moved to 9100 last week after the clash with the metrics agent on this machine.",
		"The daemon port 9100 is also what the health page of the metrics agent reports now for the daemon.",
	} {
		if _, err := srv.St.Learn(ctx, txt, "proj", "ops:port", ""); err != nil {
			t.Fatal(err)
		}
	}
	out := srv.Recall(ctx, RecallIn{Query: "daemon port", Project: "proj"})
	if wiki, notes := strings.Count(out, "[wiki "), strings.Count(out, "[memory notes/"); wiki < 1 || notes != 2 {
		t.Fatalf("default recall must hold the wiki hit and both fresh notes (wiki %d, notes %d):\n%s", wiki, notes, out)
	}
}
