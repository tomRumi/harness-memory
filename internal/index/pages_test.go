package index

import (
	"context"
	"math"
	"strings"
	"testing"
)

// pageMatrix builds a small wiki, embeds it with the bag-of-words embedder and loads the vectors. Page E is added
// after embedding, so it has no vectors.
func pageMatrix(t *testing.T) (*DB, *Matrix) {
	t.Helper()
	repo := t.TempDir()
	write(t, repo, "wiki/p/A.md", "# A\n\n## One\nalpha beta gamma delta epsilon zeta eta theta\n\n## Two\niota kappa lambda mu nu xi omicron pi\n")
	write(t, repo, "wiki/p/B.md", "# B\n\n## One\nalpha beta gamma delta epsilon zeta eta theta\n\n## Two\niota kappa lambda mu nu xi omicron rho\n")
	write(t, repo, "wiki/q/C.md", "# C\n\n## One\nalpha beta gamma delta sigma tau upsilon phi\n")
	write(t, repo, "wiki/p/D.md", "# D\n\n## One\nchi psi omega ananas banane citrom dinnye\n")
	write(t, repo, "wiki/p/Index.md", "# Index\n\n## One\nalpha beta gamma delta epsilon zeta eta theta\n")
	write(t, repo, "wiki/p/Unsorted.md", "# Unsorted\n\n## One\nalpha beta gamma delta epsilon zeta eta theta\n")
	write(t, repo, "wiki/q/A twin.md", "# A\n\n## One\nalpha beta gamma delta epsilon zeta eta theta\n\n## Two\niota kappa lambda mu nu xi omicron pi\n")
	d := openDB(t)
	if _, err := d.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EmbedPending(context.Background(), &bagEmbedder{model: "bag"}, nil); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "wiki/p/E.md", "# E\n\nno vector for this one\n")
	if _, err := d.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	m, err := d.LoadMatrix("bag")
	if err != nil {
		t.Fatal(err)
	}
	return d, m
}

func TestPageVectorIsTheNormalisedMeanOfItsChunks(t *testing.T) {
	_, m := pageMatrix(t)
	id := "wiki/p/A"
	got, ok := m.PageVector(id)
	if !ok {
		t.Fatal("no vector for A")
	}
	sum := make([]float64, m.Dim)
	n := 0
	for i, node := range m.Node {
		if node == id {
			n++
			for j, x := range m.vecs[i*m.Dim : (i+1)*m.Dim] {
				sum[j] += float64(x)
			}
		}
	}
	if n < 2 {
		t.Fatalf("A should have several chunks, has %d", n)
	}
	var norm, sq float64
	for _, x := range sum {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for j, x := range got {
		sq += float64(x * x)
		if math.Abs(float64(x)-sum[j]/norm) > 1e-5 {
			t.Fatalf("component %d: %v want %v", j, x, sum[j]/norm)
		}
	}
	if math.Abs(sq-1) > 1e-5 {
		t.Fatalf("not unit length: %v", sq)
	}
	got[0] = 99 // a copy: the cache is not changed by the caller
	if again, _ := m.PageVector(id); again[0] == 99 {
		t.Fatal("PageVector returned the cached slice")
	}
	if m.pages() != m.pages() {
		t.Fatal("page vectors must be built once per matrix")
	}
	if _, ok := m.PageVector("wiki/p/E"); ok {
		t.Fatal("a page without vectors has no page vector")
	}
	if _, ok := m.PageVector("notes/p/x"); ok {
		t.Fatal("only wiki pages have page vectors")
	}
}

func ids(ps []PageScore) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.NodeID)
	}
	return out
}

func TestRelatedPagesOrderAndExclusions(t *testing.T) {
	_, m := pageMatrix(t)
	rel, ok := m.RelatedPages("wiki/p/A", 10)
	if !ok {
		t.Fatal("A has vectors")
	}
	got := ids(rel)
	// A twin has A's text exactly (score 1) and B nearly; C shares a few words; D shares none. No A, Index, Unsorted, E.
	want := []string{"wiki/q/A twin", "wiki/p/B", "wiki/q/C", "wiki/p/D"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	for i := 1; i < len(rel); i++ {
		if rel[i].Score > rel[i-1].Score {
			t.Fatalf("not sorted by score: %v", rel)
		}
	}
	if rel[0].Score < 0.999 || rel[0].Title != "A twin" || rel[0].Project != "q" {
		t.Fatalf("first: %+v", rel[0])
	}
	if two, _ := m.RelatedPages("wiki/p/A", 2); len(two) != 2 {
		t.Fatalf("k=2 gave %d", len(two))
	}
	if _, ok := m.RelatedPages("wiki/p/E", 3); ok {
		t.Fatal("a page without vectors cannot be asked for related pages")
	}
}

func TestRelatedPagesSameProjectOnlyBreaksTies(t *testing.T) {
	_, m := pageMatrix(t)
	// Index and Unsorted are excluded, so make two equal scores out of the same text: p/A and q/A twin are equal for B.
	rel, _ := m.RelatedPages("wiki/p/B", 2)
	if rel[0].Score != rel[1].Score {
		t.Fatalf("the fixture has two copies of one page, their scores must be equal: %v", rel)
	}
	if rel[0].Project != "p" || rel[1].Project != "q" {
		t.Fatalf("same project first on equal scores: %+v", rel)
	}
	// a better page of another project still beats a worse one of the same project
	rel, _ = m.RelatedPages("wiki/q/C", 4)
	if rel[len(rel)-1].NodeID != "wiki/p/D" {
		t.Fatalf("the unrelated page must be last: %v", ids(rel))
	}
}

func TestNearDuplicatesThresholdAndEachPairOnce(t *testing.T) {
	_, m := pageMatrix(t)
	pairs := m.NearDuplicates("", 0.6)
	if len(pairs) != 3 { // A~A twin (1.0), B~A, B~A twin
		t.Fatalf("pairs: %+v", pairs)
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		k := p.A.NodeID + "|" + p.B.NodeID
		if seen[k] || seen[p.B.NodeID+"|"+p.A.NodeID] || p.A.NodeID == p.B.NodeID {
			t.Fatalf("pair twice or with itself: %+v", pairs)
		}
		seen[k] = true
		if p.Score < 0.6 || IsSpecialPage(p.A.NodeID) || IsSpecialPage(p.B.NodeID) {
			t.Fatalf("bad pair %+v", p)
		}
	}
	if pairs[0].Score < pairs[len(pairs)-1].Score {
		t.Fatal("best pair first")
	}
	if got := m.NearDuplicates("", 0.9999); len(got) != 1 || got[0].A.NodeID != "wiki/p/A" || got[0].B.NodeID != "wiki/q/A twin" {
		t.Fatalf("only the exact copy at 0.9999: %+v", got)
	}
	if got := m.NearDuplicates("p", 0.6); len(got) != 1 { // A~B only: both pages in p
		t.Fatalf("project filter: %+v", got)
	}
	if got := m.NearDuplicates("", 1.1); len(got) != 0 {
		t.Fatalf("threshold above 1: %+v", got)
	}
}

func TestNodeVectorEmbedsMissingChunksAndResolvePage(t *testing.T) {
	d, m := pageMatrix(t)
	want, _ := m.PageVector("wiki/p/A")
	got, err := d.NodeVector(context.Background(), &bagEmbedder{model: "bag"}, "wiki/p/A")
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if math.Abs(float64(want[i]-got[i])) > 1e-5 {
			t.Fatal("stored vectors and NodeVector must agree")
		}
	}
	e := &bagEmbedder{model: "bag"}
	if _, err := d.NodeVector(context.Background(), e, "wiki/p/E"); err != nil || e.calls.Load() == 0 {
		t.Fatalf("a chunk without a vector is embedded: %v calls %d", err, e.calls.Load())
	}
	e.fail.Store(true)
	if _, err := d.NodeVector(context.Background(), e, "wiki/p/E"); err == nil {
		t.Fatal("embedder down and no stored vector must be an error")
	}
	if _, err := d.NodeVector(context.Background(), nil, "wiki/p/A"); err == nil {
		t.Fatal("no embedder: the model is unknown, so nothing is found")
	}
	for in, want := range map[string]string{"p/A.md": "wiki/p/A", "wiki/p/A.md": "wiki/p/A", "A twin": "wiki/q/A twin", "q/c": "wiki/q/C"} {
		if id, err := d.ResolvePage(in); err != nil || id != want {
			t.Fatalf("ResolvePage(%q) = %q, %v", in, id, err)
		}
	}
	if _, err := d.ResolvePage("Nowhere"); err == nil {
		t.Fatal("unknown page")
	}
}

// centredWiki makes 12 pages that all share most of their words (one style, one subject area) and differ in topic words.
func centredWiki(t *testing.T) *Matrix {
	t.Helper()
	repo := t.TempDir()
	common := "the system module handles requests and keeps state in a store with a queue and a worker that retries failed jobs "
	topics := [][2]string{{"alpha", "bravo"}, {"alpha", "bravo"}, {"charlie", "delta"}, {"charlie", "delta"}, {"echo", "foxtrot"}, {"echo", "foxtrot"},
		{"golf", "hotel"}, {"golf", "hotel"}, {"india", "juliet"}, {"india", "juliet"}, {"kilo", "lima"}, {"kilo", "lima"}}
	for i, tp := range topics {
		body := "# Page" + string(rune('A'+i)) + "\n\n" + common + strings.Repeat(tp[0]+" "+tp[1]+" ", 6) + "\n"
		write(t, repo, "wiki/p/Page"+string(rune('A'+i))+".md", body)
	}
	d := openDB(t)
	if _, err := d.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EmbedPending(context.Background(), &bagEmbedder{model: "bag"}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := d.LoadMatrix("bag")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPageVectorsAreCentredFromEightPagesUp(t *testing.T) {
	m := centredWiki(t)
	if m.pages().mean == nil {
		t.Fatal("12 pages must be centred")
	}
	// raw cosines of this wiki are high for every pair (shared words); centred ones separate the topics
	same, _ := m.Cosine("wiki/p/PageA", "wiki/p/PageB")  // same topic words
	other, _ := m.Cosine("wiki/p/PageA", "wiki/p/PageE") // other topic words
	if !(same > 0.3 && other < 0 && m.RelatedMin() == RelatedFloor) {
		t.Fatalf("centred: same topic %v, other topic %v, floor %v", same, other, m.RelatedMin())
	}
	// every page vector has unit length
	for _, id := range m.pages().ids {
		var n float64
		for _, x := range m.pages().vec[id] {
			n += float64(x * x)
		}
		if math.Abs(n-1) > 1e-4 {
			t.Fatalf("%s has length^2 %v", id, n)
		}
	}
	// related pages use the same space: A's best match is B
	rel, _ := m.RelatedPages("wiki/p/PageA", 1)
	if len(rel) != 1 || rel[0].NodeID != "wiki/p/PageB" {
		t.Fatalf("related: %+v", rel)
	}
}

func TestSmallWikiKeepsRawVectors(t *testing.T) {
	_, m := pageMatrix(t)
	if m.pages().mean != nil || m.RelatedMin() != RelatedFloorRaw {
		t.Fatal("a wiki below CenterMinPages must keep raw page vectors and the raw floor")
	}
}
