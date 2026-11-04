package index

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bagEmbedder is a deterministic stand-in for a model: a hashed bag of words, unit length. Texts that share words are close.
type bagEmbedder struct {
	model string
	calls atomic.Int64
	fail  atomic.Bool
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func (b *bagEmbedder) vec(text string) []float32 {
	v := make([]float32, 64)
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%64]++
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
func (b *bagEmbedder) Model() string { return b.model }
func (b *bagEmbedder) ID() string    { return b.model }
func (b *bagEmbedder) Docs(ctx context.Context, texts []string) ([][]float32, error) {
	b.calls.Add(int64(len(texts)))
	if b.fail.Load() {
		return nil, errors.New("embedder down")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = b.vec(t)
	}
	return out, nil
}
func (b *bagEmbedder) Query(ctx context.Context, text string) ([]float32, error) {
	return b.vec(text), nil
}

func TestEmbedPendingIsResumableAndSurvivesAnIndexRebuild(t *testing.T) {
	repo, _ := fixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Rescan(repo)
	e := &bagEmbedder{model: "bag"}
	miss, total, _ := d.Pending("bag")
	if miss != total || total == 0 {
		t.Fatalf("pending %d of %d", miss, total)
	}
	n, err := d.EmbedPending(context.Background(), e, nil)
	if err != nil || n != total {
		t.Fatalf("embedded %d of %d: %v", n, total, err)
	}
	if n, _ := d.EmbedPending(context.Background(), e, nil); n != 0 {
		t.Fatalf("a second run must embed nothing, got %d", n)
	}
	d.Close()
	// delete the index but not the vector cache: the rebuild needs no embedding at all
	os.Remove(path)
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Rescan(repo)
	before := e.calls.Load()
	if miss, _, _ := d.Pending("bag"); miss != 0 {
		t.Fatalf("after an index rebuild %d texts lack a vector", miss)
	}
	if n, _ := d.EmbedPending(context.Background(), e, nil); n != 0 || e.calls.Load() != before {
		t.Fatal("rebuilding the index re-embedded text")
	}
	// a changed file needs exactly its new chunks embedded
	write(t, repo, "wiki/alpha/setup.md", "# Setup\n\n## Steps\nCreate the frobnicator directory, then restart the daemon.\n")
	d.Rescan(repo)
	if n, _ := d.EmbedPending(context.Background(), e, nil); n == 0 || n > 3 {
		t.Fatalf("expected only the edited entry to be embedded, got %d", n)
	}
	// another model starts from zero, and DropVectors removes the old one
	if miss, total, _ := d.Pending("other"); miss != total {
		t.Fatalf("other model pending %d of %d", miss, total)
	}
	d.EmbedPending(context.Background(), &bagEmbedder{model: "other"}, nil)
	if n, _ := d.DropVectors("other"); n == 0 {
		t.Fatal("vectors of the old model were not dropped")
	}
}

func TestEmbedPendingStopsOnErrorAndKeepsWhatWasDone(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	e := &bagEmbedder{model: "bag"}
	e.fail.Store(true)
	if _, err := d.EmbedPending(context.Background(), e, nil); err == nil {
		t.Fatal("an embedder failure must be an error")
	}
	e.fail.Store(false)
	miss, total, _ := d.Pending("bag")
	if miss != total {
		t.Fatalf("a failed run stored vectors: %d of %d", miss, total)
	}
	if n, err := d.EmbedPending(context.Background(), e, nil); err != nil || n != total {
		t.Fatalf("rerun: %d %v", n, err)
	}
}

func TestHybridFindsByMeaningAndFallsBack(t *testing.T) {
	repo, _ := fixture(t)
	write(t, repo, "wiki/alpha/lifecycle.md", "# Lifecycle\n\nProcesses claim a lease on each session so two of them cannot work on it together.\n")
	d := openDB(t)
	d.Rescan(repo)
	e := &bagEmbedder{model: "bag"}
	// no vectors yet: the hybrid result is the keyword result
	m0, _ := d.LoadMatrix("bag")
	if m0.Missing == 0 {
		t.Fatal("Missing must count chunks without a vector")
	}
	_, via, _ := d.Hybrid(context.Background(), e, m0, "lease session", Filter{Limit: 3})
	if via != "keyword" {
		t.Fatalf("via %q", via)
	}
	d.EmbedPending(context.Background(), e, nil)
	m, err := d.LoadMatrix("bag")
	if err != nil || m.Missing != 0 || m.Dim != 64 {
		t.Fatalf("%+v %v", m, err)
	}
	hits, via, warn := d.Hybrid(context.Background(), e, m, "which lease does a session claim", Filter{Limit: 3})
	if warn != nil || via != "hybrid" || len(hits) == 0 || hits[0].NodeID != "wiki/alpha/lifecycle" {
		t.Fatalf("via %s warn %v hits %+v", via, warn, hits)
	}
	// filters apply to the vector pass as well
	if hits, _, _ := d.Hybrid(context.Background(), e, m, "lease session", Filter{Limit: 3, Kinds: []string{"fact"}}); len(hits) != 0 {
		for _, h := range hits {
			if h.Kind != "fact" {
				t.Fatalf("filter ignored: %+v", h)
			}
		}
	}
	// retired nodes never come back through the vector pass
	if hits, _, _ := d.Hybrid(context.Background(), e, m, "old retired claim about quibblewomp", Filter{Limit: 5}); func() bool {
		for _, h := range hits {
			if h.NodeID == "notes/alpha/n-0002" {
				return true
			}
		}
		return false
	}() {
		t.Fatal("a retired node was returned")
	}
	// a matrix built for another model dimension is refused with a clear message
	if _, err := m.TopK(make([]float32, 8), 3, Filter{}); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("dimension mismatch: %v", err)
	}
}

func TestTitleAndPathAreSearchable(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	hits, _ := d.Keyword("main", Filter{Kinds: []string{"code"}})
	if len(hits) == 0 || hits[0].Path != "src/main.py" {
		t.Fatalf("a code file must be found by its name: %+v", hits)
	}
}

func TestOldSchemaIsRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	d, _ := Open(path)
	d.Exec(`PRAGMA user_version = 1`)
	d.Exec(`INSERT INTO nodes(id,kind,project,path,title,status,retired_reason,superseded_by,pinned,note_kind,created_at,updated_at,content_hash) VALUES('x','wiki','p','p','t','active','','',0,'','','','h')`)
	d.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	d.QueryRow(`SELECT count(*) FROM nodes`).Scan(&n)
	if n != 0 {
		t.Fatal("an index of an older schema must be dropped, it is derived")
	}
}

func TestBestKeywordHitsKeepAPlace(t *testing.T) {
	repo, _ := fixture(t)
	// The exact entry holds the identifier and little else. Ten others repeat the query's ordinary words, so they are
	// closer to the query in meaning and also match its words; only the exact entry matches the rare identifier.
	write(t, repo, "wiki/alpha/exact.md", "# Exact\n\nquux_handler_7 is a thing.\n")
	for i := 0; i < 10; i++ {
		write(t, repo, filepath.Join("wiki/alpha", "near"+string(rune('a'+i))+".md"),
			"# Near\n\nhandler function drops stale rows. handler function drops stale rows. handler function drops stale rows.\n")
	}
	d := openDB(t)
	d.Rescan(repo)
	e := &bagEmbedder{model: "bag"}
	d.EmbedPending(context.Background(), e, nil)
	m, _ := d.LoadMatrix("bag")
	q := "quux_handler_7 handler function drops stale rows handler function drops stale rows"
	hits, _, _ := d.Hybrid(context.Background(), e, m, q, Filter{Limit: 3, Kinds: []string{"wiki"}})
	found := false
	for _, h := range hits {
		found = found || h.NodeID == "wiki/alpha/exact"
	}
	if !found || len(hits) != 3 {
		t.Fatalf("the exact keyword match must keep a place: %+v", hits)
	}
}

type slowEmbedder struct{ bagEmbedder }

func (s *slowEmbedder) Query(ctx context.Context, text string) ([]float32, error) {
	s.calls.Add(1000)
	<-ctx.Done() // a model server that hangs
	return nil, ctx.Err()
}

func TestHungEmbedderFallsBackToKeywordsQuickly(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	good := &bagEmbedder{model: "bag"}
	d.EmbedPending(context.Background(), good, nil)
	m, _ := d.LoadMatrix("bag")
	slow := &slowEmbedder{bagEmbedder{model: "bag"}}
	start := time.Now()
	hits, via, warn := d.Hybrid(context.Background(), slow, m, "frobnicator directory", Filter{Limit: 3, Kinds: []string{"wiki"}})
	if el := time.Since(start); el > QueryEmbedTimeout+time.Second {
		t.Fatalf("recall took %s with a hung embedder", el)
	}
	if warn == nil || via != "keyword" || len(hits) == 0 {
		t.Fatalf("via %q warn %v hits %d", via, warn, len(hits))
	}
	// a code-only recall never calls the model at all
	before := slow.calls.Load()
	d.Hybrid(context.Background(), slow, m, "compute_total", Filter{Limit: 3, Kinds: []string{"code"}})
	if slow.calls.Load() != before {
		t.Fatal("the embedder was called for a code-only recall")
	}
}

func TestPruneIsSafe(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	// an empty index must not delete the vector cache
	d.Exec(`INSERT INTO vec.vectors(text_hash, model, dim, vec) VALUES('h1','bag',2,x'0000000000000000')`)
	if _, err := d.DropVectors("bag"); err == nil {
		t.Fatal("pruning an empty index must be refused")
	}
	var n int
	d.QueryRow(`SELECT count(*) FROM vec.vectors`).Scan(&n)
	if n != 1 {
		t.Fatalf("the cache was emptied: %d", n)
	}
	d.Rescan(repo)
	d.EmbedPending(context.Background(), &bagEmbedder{model: "bag"}, nil)
	if k, err := d.DropVectors("bag"); err != nil || k < 1 { // the unrelated 'h1' vector is pruned, the real ones stay
		t.Fatalf("%d %v", k, err)
	}
	if miss, _, _ := d.Pending("bag"); miss != 0 {
		t.Fatalf("prune removed vectors that are in use: %d missing", miss)
	}
}

func TestRetiredNodesAreNotEmbeddedAndTopKGuardsK(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	e := &bagEmbedder{model: "bag"}
	d.EmbedPending(context.Background(), e, nil)
	var retiredText string
	d.QueryRow(`SELECT c.text FROM chunks c JOIN nodes n ON n.id=c.node_id WHERE n.status='retired'`).Scan(&retiredText)
	var has int
	d.QueryRow(`SELECT count(*) FROM vec.vectors v JOIN chunks c ON c.text_hash=v.text_hash JOIN nodes n ON n.id=c.node_id WHERE n.status='retired'`).Scan(&has)
	if has != 0 {
		t.Fatal("a retired note was embedded")
	}
	m, _ := d.LoadMatrix("bag")
	if ids, err := m.TopK(make([]float32, m.Dim), 0, Filter{}); err != nil || ids != nil {
		t.Fatalf("k=0 must return nothing: %v %v", ids, err)
	}
}

func TestVectorFileSurvivesAReplacedConnection(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	d.EmbedPending(context.Background(), &bagEmbedder{model: "bag"}, nil)
	d.SetConnMaxLifetime(time.Millisecond) // database/sql opens a fresh connection for the next call
	time.Sleep(20 * time.Millisecond)
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM vec.vectors`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("vector table lost after the connection was replaced: %d %v", n, err)
	}
}
