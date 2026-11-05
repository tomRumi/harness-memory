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
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

type bag struct{ down bool }

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
	if b.down {
		return nil, fmt.Errorf("embedder down")
	}
	o := make([][]float32, len(t))
	for i, x := range t {
		o[i] = b.vec(x)
	}
	return o, nil
}
func (b *bag) Query(ctx context.Context, t string) ([]float32, error) {
	if b.down {
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

func TestSevenToolsAndTheirSchemaSize(t *testing.T) {
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
	want := "fact_add feedback learn recall retire stats wiki_read"
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
	if !strings.Contains(out, "[code: not built for proj]") {
		t.Fatalf("empty layer must say 'not built':\n%s", out)
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
	e.down = true
	out, isErr := call(t, s, "recall", map[string]any{"query": "daemon port", "project": "proj", "layer": "wiki"})
	if isErr || !strings.Contains(out, "keyword match only") || !strings.Contains(out, "8765") {
		t.Fatalf("recall must fall back to keywords and say so: %v\n%s", isErr, out)
	}
	st := srv.Stats(context.Background(), StatsIn{})
	if !strings.Contains(st, "embedder: unavailable") {
		t.Fatalf("stats must report the embedder state:\n%s", st)
	}
	if _, isErr := call(t, s, "learn", map[string]any{"text": "A note while down.", "kind": "k"}); !isErr {
		t.Fatal("learn must fail loudly while the embedder is down")
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
	if out, _ := call(t, s, "retire", map[string]any{"id": short, "reason": "obsolete", "project": "proj"}); !strings.HasPrefix(out, "retired "+short) {
		t.Fatalf("%s", out)
	}
	out, _ = call(t, s, "recall", map[string]any{"query": "weekly job sunday", "project": "proj", "layer": "memory"})
	if strings.Contains(out, "#"+short+"]") {
		t.Fatalf("retired note is still recalled:\n%s", out)
	}
	out, _ = call(t, s, "recall", map[string]any{"query": "weekly job sunday", "project": "proj", "layer": "memory", "include_archived": true})
	if !strings.Contains(out, "(retired)") {
		t.Fatalf("include_archived must show the retired note, labelled:\n%s", out)
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
	for _, want := range []string{"harness-memory-service", "wiki/proj", "embedder: bag ok", "commits: ok"} {
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
	if resp.StatusCode != 200 || !strings.Contains(string(b), "embedder: bag ok") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestContextBlockKeepsOnlyRealHits(t *testing.T) {
	_, full, _, _, _ := setup(t)
	srv, _ := NewReader(full.DB, nil) // keyword only, as the hook runs by default
	if got := srv.ContextBlock(context.Background(), "zzzqqq nothing matches this", "", 1000); got != "" {
		t.Fatalf("a query without a hit must give an empty block, got %q", got)
	}
	got := srv.ContextBlock(context.Background(), "daemon port loopback", "", 1000)
	if !strings.HasPrefix(got, "<memory-context") || !strings.HasSuffix(got, "</memory-context>") || strings.Contains(got, "no results") || strings.Contains(got, "not built") {
		t.Fatalf("%q", got)
	}
}
