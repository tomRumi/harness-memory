package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory/internal/index"
)

// putPage writes a wiki page the way a hand edit or the later merge flow does (a file), then lets the store index it
// and tell the committer.
func putPage(t *testing.T, st *Store, rel, content string) {
	t.Helper()
	p := filepath.Join(st.Repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.afterWrite(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
}

func TestWikiReadByPathAndByTitleWithLinksFooter(t *testing.T) {
	st, _, _ := newStore(t)
	putPage(t, st, "wiki/p/Hub page.md", "---\nsummary: hub\naliases: [Center]\n---\nSee [[Leaf]] and [[q/Remote]] and [[Nowhere]].\n")
	putPage(t, st, "wiki/p/Leaf.md", "Back to [[hub PAGE]].\n")
	putPage(t, st, "wiki/q/Remote.md", "Remote, links [[p/Hub page]].\n")
	putPage(t, st, "wiki/q/Lonely.md", "No links.\n")
	wantHub := "Links to: [[Leaf]], [[q/Remote]]\nLinked from: [[Leaf]], [[q/Remote]]\n"
	for _, in := range []string{"p/Hub page.md", "wiki/p/Hub page.md", "Hub page", "hub PAGE", "p/hub page", "Center", "wiki/p/Hub page", "HUB PAGE.md"} {
		got, err := st.WikiRead(in)
		if err != nil {
			t.Fatalf("WikiRead(%q): %v", in, err)
		}
		if !strings.HasPrefix(got, "---\nsummary: hub") || !strings.HasSuffix(got, "\n---\n"+wantHub) {
			t.Fatalf("WikiRead(%q):\n%s", in, got)
		}
	}
	leaf, _ := st.WikiRead("Leaf")
	if !strings.HasSuffix(leaf, "Links to: [[Hub page]]\nLinked from: [[Hub page]]\n") {
		t.Fatalf("leaf:\n%s", leaf)
	}
	remote, _ := st.WikiRead("q/Remote")
	if !strings.HasSuffix(remote, "Links to: [[p/Hub page]]\nLinked from: [[p/Hub page]]\n") {
		t.Fatalf("remote:\n%s", remote)
	}
	lonely, _ := st.WikiRead("Lonely")
	if lonely != "No links.\n" {
		t.Fatalf("a page without links gets no footer: %q", lonely)
	}
	only := putOnlyIn(t, st)
	if !strings.HasSuffix(only, "Linked from: [[Hub page]]\n") || strings.Contains(only, "Links to:") {
		t.Fatalf("an empty line is left out:\n%s", only)
	}
	if _, err := st.WikiRead("Nowhere"); err == nil {
		t.Fatal("an unknown title must be an error")
	}
	putPage(t, st, "wiki/p/Twin.md", "t\n")
	putPage(t, st, "wiki/q/Twin.md", "t\n")
	if _, err := st.WikiRead("twin"); err == nil || !strings.Contains(err.Error(), "wiki/p/Twin") || !strings.Contains(err.Error(), "wiki/q/Twin") {
		t.Fatalf("ambiguous title: %v", err)
	}
	if got, err := st.WikiRead("p/twin"); err != nil || !strings.HasPrefix(got, "t\n") { // the twin has the same text, so it may be listed as related
		t.Fatalf("explicit project: %q %v", got, err)
	}
	if got, err := st.WikiRead("Wait..."); err == nil {
		t.Fatalf("%q", got)
	}
}

func putOnlyIn(t *testing.T, st *Store) string {
	putPage(t, st, "wiki/p/Target only.md", "target\n")
	putPage(t, st, "wiki/p/Hub page.md", "---\nsummary: hub\naliases: [Center]\n---\nSee [[Leaf]] and [[q/Remote]] and [[Nowhere]] and [[Target only]].\n")
	got, err := st.WikiRead("Target only")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWriteIndexPageWritesOnceThenSaysUnchangedAndCommitsNothing(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	putPage(t, st, "wiki/p/Beta.md", "---\nsummary: Second\n---\nx\n")
	putPage(t, st, "wiki/p/alpha.md", "Alpha explains things. Then more.\n")
	putPage(t, st, "wiki/q/Other.md", "other\n")
	st.Commit.Flush()
	wrote, err := st.WriteIndexPage(ctx, "p")
	if err != nil || !wrote {
		t.Fatalf("first call: %v %v", wrote, err)
	}
	b, err := os.ReadFile(filepath.Join(repo, "wiki/p/Index.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "---\ngenerated: true\ntitle: Index\n---\n\n# Index\n\n- [[alpha]] - Alpha explains things.\n- [[Beta]] - Second\n"
	if string(b) != want {
		t.Fatalf("index page:\n%s\nwant\n%s", b, want)
	}
	st.Commit.Flush()
	commits := gitOut(t, repo, "rev-list", "--count", "HEAD")
	if !strings.Contains(gitOut(t, repo, "-c", "core.quotepath=off", "show", "--name-only", "--format=%s", "HEAD"), "wiki/p/Index.md") {
		t.Fatal("the index page was not committed")
	}
	// searchable, a normal node, but not listed in itself
	hits, _ := st.DB.Keyword("generated Index", index.Filter{Project: "p", Kinds: []string{"wiki"}})
	if len(hits) == 0 || !hasNode(hits, "wiki/p/Index") {
		t.Fatalf("the index page is not found by recall: %+v", hits)
	}
	again, err := st.WriteIndexPage(ctx, "p")
	if err != nil || again {
		t.Fatalf("second call must say unchanged: %v %v", again, err)
	}
	st.Commit.Flush()
	if n := gitOut(t, repo, "rev-list", "--count", "HEAD"); n != commits {
		t.Fatalf("a second call made a commit: %s -> %s", commits, n)
	}
	// a new page changes the index; the other project's index is untouched
	putPage(t, st, "wiki/p/Gamma.md", "Gamma.\n")
	if wrote, _ := st.WriteIndexPage(ctx, "p"); !wrote {
		t.Fatal("a new page must change the index")
	}
	if _, err := os.Stat(filepath.Join(repo, "wiki/q/Index.md")); !os.IsNotExist(err) {
		t.Fatal("project q must not get an index page unasked")
	}
	for _, bad := range []string{"", "../x", "a/b", ".hidden"} {
		if _, err := st.WriteIndexPage(ctx, bad); err == nil {
			t.Fatalf("project %q must be refused", bad)
		}
	}
}

func TestTitleWithSpacesAndAccentsIsWrittenCommittedAndRecalled(t *testing.T) {
	st, _, repo := newStore(t)
	rel := "wiki/proj/Őrzés és café – jegyzet.md"
	putPage(t, st, rel, "---\nsummary: Egy oldal\n---\n\n## Rész egy\nA zebrafish_marker szöveg itt van.\n")
	st.Commit.Flush()
	if err := st.Commit.LastErr(); err != nil {
		t.Fatal(err)
	}
	log := gitOut(t, repo, "-c", "core.quotepath=off", "log", "--name-only", "--format=%s")
	if !strings.Contains(log, rel) {
		t.Fatalf("git log does not show the path:\n%s", log)
	}
	if gitOut(t, repo, "status", "--porcelain") != "" {
		t.Fatalf("uncommitted: %s", gitOut(t, repo, "status", "--porcelain"))
	}
	hits, err := st.DB.Keyword("zebrafish_marker", index.Filter{})
	if err != nil || len(hits) != 1 || hits[0].Path != rel || hits[0].Section != "Rész egy" {
		t.Fatalf("recall by text: %+v %v", hits, err)
	}
	if hits, _ := st.DB.Keyword("café jegyzet", index.Filter{}); len(hits) == 0 || hits[0].Path != rel {
		t.Fatalf("recall by title: %+v", hits)
	}
	got, err := st.WikiRead("őrzés ÉS café – jegyzet")
	if err != nil || !strings.Contains(got, "zebrafish_marker") {
		t.Fatalf("read by title: %q %v", got, err)
	}
	// delete: the deletion of a path with spaces and accents is committed as well
	os.Remove(filepath.Join(repo, rel))
	st.afterWrite(context.Background(), rel)
	st.Commit.Flush()
	if gitOut(t, repo, "status", "--porcelain") != "" || st.Commit.LastErr() != nil {
		t.Fatalf("deletion not committed: %s %v", gitOut(t, repo, "status", "--porcelain"), st.Commit.LastErr())
	}
}

func TestWikiReadRelatedLine(t *testing.T) {
	st, e, _ := newStore(t)
	const same = "The scheduler retries failed jobs three times and then alerts the owner.\n"
	putPage(t, st, "wiki/p/Alpha.md", same)
	putPage(t, st, "wiki/q/Beta.md", same)
	putPage(t, st, "wiki/p/Gamma.md", "Bananas oranges lemons pears cherries plums.\n")
	putPage(t, st, "wiki/p/Index.md", same)
	got, err := st.WikiRead("Alpha")
	if err != nil || !strings.HasSuffix(got, "\n---\nRelated: [[q/Beta]] (1.00)\n") {
		t.Fatalf("Alpha: %v\n%q", err, got) // Gamma is under the floor, Index is never listed
	}
	if got, _ := st.WikiRead("Gamma"); strings.Contains(got, "Related:") {
		t.Fatalf("a page with nothing close gets no line: %q", got)
	}
	if got, _ := st.WikiRead("p/Index"); strings.Contains(got, "Related:") {
		t.Fatalf("an Index page gets no line: %q", got)
	}
	// the embedder goes down: the new page has no vectors, the stored ones are still used, and reading never fails
	e.fail.Store(true)
	putPage(t, st, "wiki/p/Delta.md", "Freshly written words nobody embedded yet.\n") // vectors are keyed by text: a copy would reuse them
	if got, err := st.WikiRead("Delta"); err != nil || strings.Contains(got, "Related:") {
		t.Fatalf("a page without vectors gets no line: %v %q", err, got)
	}
	if got, err := st.WikiRead("Alpha"); err != nil || !strings.HasSuffix(got, "Related: [[q/Beta]] (1.00)\n") {
		t.Fatalf("Alpha with the embedder down: %v %q", err, got)
	}
	// no embedder at all
	st.Emb = nil
	if got, err := st.WikiRead("Alpha"); err != nil || strings.Contains(got, "Related:") {
		t.Fatalf("without an embedder: %v %q", err, got)
	}
	// a matrix handed in by the server is used as it is
	st.Matrix = func() *index.Matrix { m, _ := st.DB.LoadMatrix("bag"); return m }
	if got, _ := st.WikiRead("Alpha"); !strings.Contains(got, "Related: [[q/Beta]] (1.00)") {
		t.Fatalf("shared matrix: %q", got)
	}
}
