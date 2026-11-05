package curate

// Second review: adversarial tests of the new curate, md and NFC code. "DEFECT" marks a finding.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

// allWiki is the text of every wiki file except the generated Index.
func allWiki(t *testing.T, repo string) string {
	var b strings.Builder
	filepath.Walk(filepath.Join(repo, "wiki"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && fi.Name() != "Index.md" {
			d, _ := os.ReadFile(p)
			b.WriteString(string(d))
		}
		return nil
	})
	return b.String()
}

// N01: titles that the macOS file system treats as one name but wiki.Fold (NFC + ToLower) does not.
func TestN01_CaseFoldCollisionOverwritesPage(t *testing.T) {
	for _, c := range []struct{ old, new string }{
		{"Strasse", "Straße"},
		{"ΟΔΟΣ", "οδος"},
		{"file", "ﬁle"},
	} {
		st, repo := newStore(t)
		old := "# " + c.old + "\n\nYears of hand-written text that must survive the merge.\n"
		putFile(t, st, "wiki/proj/"+c.old+".md", old)
		ns := learnN(t, st, proj, 1, tmplWorker)
		rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: c.new, Summary: "s", Body: "# T\n\n" + bullets(ns), Notes: ids(ns)}}})
		t.Logf("%s -> %s: err=%v pages=%+v", c.old, c.new, err, rep.Pages)
		if !strings.Contains(allWiki(t, repo), "Years of hand-written text") {
			t.Errorf("DEFECT existing page %q overwritten by new title %q with no base_hash; git status: %q", c.old, c.new, gitStatus(repo))
		}
	}
}

// N02: the same within one submission.
func TestN02_CaseFoldCollisionInOneSubmission(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 2, tmplWorker)
	Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Strasse", Summary: "s", Body: "# S\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "Straße", Summary: "s", Body: "# S\n\n" + bullets(ns[1:]), Notes: ids(ns[1:])},
	}})
	for _, n := range ns {
		if !textSomewhere(t, repo, proj, n) {
			t.Errorf("DEFECT note %s deleted; the two titles are one file on APFS", short(n.ID))
		}
	}
}

// N03: links and aliases across normalisation resolve.
func TestN03_LinksAndAliasesNFC(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "wiki/proj/Caf\u00e9.md", "---\naliases: [Kave\u0301]\n---\n# Café\n\ntext about the cafe.\n")
	ns := learnN(t, st, proj, 1, tmplWorker)
	old := "# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", old)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Ports", Body: old + "\nSee [[Cafe\u0301]] and [[Kav\u00e9]].\n\n" + bullets(ns), Notes: ids(ns), BaseHash: Hash([]byte(old))}}})
	if rep.Pages[0].Status != Merged {
		t.Errorf("DEFECT links across NFC/NFD (title or alias) refused: %+v", rep.Pages)
	}
}

// N04: a merge that opens ``` and "closes" with ~~~ (or opens ```` and closes ```) adds 2 fence lines, so the rule
// passes, the block stays open to the end of the page, and a new broken link after it is not seen.
func TestN04_FenceLeftOpenByMismatchedMarkers(t *testing.T) {
	old := "# P\n\nOld text of the page.\n"
	notes := []Note{{ID: "n1", Text: "The relay listens on port 9123 for the agents."}}
	for name, body := range map[string]string{
		"``` then ~~~":  old + "\n```\nThe relay listens on port 9123 for the agents.\n~~~\n\nSee [[Nowhere At All]].\n",
		"```` then ```": old + "\n````\nThe relay listens on port 9123 for the agents.\n```\n\nSee [[Nowhere At All]].\n",
	} {
		probs := CheckPage(old, body, notes, func(string) bool { return false })
		var f md.Fence
		for _, l := range strings.Split(body, "\n") {
			f.Line(l)
		}
		if len(probs) == 0 {
			t.Errorf("DEFECT %s: accepted; fence open at end=%v; the new broken link is hidden", name, f.Open())
		}
	}
}

// N06: SetKeys on block lists with a blank line or a comment between the items.
func TestN06_SetKeysBlockListVariants(t *testing.T) {
	for name, src := range map[string]string{
		"blank line inside": "---\ntitle: P\ntags:\n- a\n\n- b\nsummary: s\n---\nbody\n",
		"comment inside":    "---\ntitle: P\ntags:\n- a\n# c\n- b\nsummary: s\n---\nbody\n",
		"flow then block":   "---\ntitle: P\ntags: [a]\nsources:\n- x\n---\nbody\n",
	} {
		if d := md.Parse([]byte(src)); d.Error != "" {
			t.Logf("%s: source itself does not parse: %s", name, d.Error)
			continue
		}
		out, err := md.SetKeys([]byte(src), map[string]any{"tags": []string{"a", "b", "c"}, "summary": "new"})
		if err != nil {
			t.Logf("%s: SetKeys refused: %v", name, err)
			continue
		}
		if d := md.Parse(out); d.Error != "" {
			t.Errorf("DEFECT %s: frontmatter broken after SetKeys: %s\n%s", name, d.Error, out)
		}
	}
}

// N08: an Index.md the owner cannot read is taken as absent and overwritten.
func TestN08_UnreadableHandWrittenIndex(t *testing.T) {
	st, repo := newStore(t)
	p := filepath.Join(repo, "wiki", proj, "Index.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("# My index\n\nhand written\n"), 0o200)
	ns := learnN(t, st, proj, 1, tmplWorker)
	Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	os.Chmod(p, 0o644)
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "hand written") {
		t.Errorf("minor DEFECT an unreadable hand-written Index.md was overwritten")
	}
}

// --- tests added with the fixes -------------------------------------------------------------------------------

// N01b: the new title that folds to an existing page is that page: without its base_hash the submission is stale,
// the page stays as it was and the note stays waiting. Several folds a file system treats as one name.
func TestCaseFoldedTitleIsTheExistingPage(t *testing.T) {
	for _, c := range []struct{ old, new string }{
		{"Strasse", "Straße"}, {"ΟΔΟΣ", "οδος"}, {"file", "ﬁle"}, {"µ", "μ"}, {"ſ", "s"}, {"Ports", "PORTS"},
	} {
		st, repo := newStore(t)
		putFile(t, st, "wiki/proj/"+c.old+".md", "# "+c.old+"\n\nhand text\n")
		ns := learnN(t, st, proj, 1, tmplWorker)
		rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: c.new, Summary: "s", Body: "# T\n\n" + bullets(ns), Notes: ids(ns)}}})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Pages[0].Status != Stale {
			t.Errorf("%q over %q: status %q, want stale", c.new, c.old, rep.Pages[0].Status)
		}
		if _, err := os.Stat(filepath.Join(repo, "notes", proj, ns[0].ID+".md")); err != nil {
			t.Errorf("%q over %q: the note is gone", c.new, c.old)
		}
		// with the base hash it is a merge into the old page, under the old file name
		old := "# " + c.old + "\n\nhand text\n"
		rep, _ = Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: c.new, Summary: "s", Body: old + "\n" + bullets(ns), Notes: ids(ns), BaseHash: Hash([]byte(old))}}})
		if rep.Pages[0].Status != Merged {
			t.Errorf("%q over %q with base_hash: %+v", c.new, c.old, rep.Pages)
		}
		if _, err := os.Stat(filepath.Join(repo, "wiki", proj, c.old+".md")); err != nil {
			t.Errorf("%q over %q: the old file name is gone", c.new, c.old)
		}
	}
}

// N02b: two titles of one submission that fold equal: the second page is refused, its notes go to Unsorted.
func TestSecondFoldedTitleGoesToUnsorted(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 2, tmplWorker)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Strasse", Summary: "s", Body: "# S\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "Straße", Summary: "s", Body: "# S\n\n" + bullets(ns[1:]), Notes: ids(ns[1:])},
	}})
	if rep.Pages[0].Status != Merged || rep.Pages[1].Status != Refused {
		t.Fatalf("statuses: %+v", rep.Pages)
	}
	un, _ := os.ReadFile(filepath.Join(repo, "wiki", proj, "Unsorted.md"))
	if !strings.Contains(string(un), ns[1].Text) {
		t.Errorf("the note of the refused page is not in Unsorted:\n%s", un)
	}
	for _, n := range ns {
		if !textSomewhere(t, repo, proj, n) {
			t.Errorf("note %s lost", short(n.ID))
		}
	}
}

// A link resolves with the same fold.
func TestLinkResolvesWithFullCaseFolding(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "wiki/proj/Strasse.md", "# Strasse\n\ntext\n")
	ns := learnN(t, st, proj, 1, tmplWorker)
	old := "# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", old)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Ports", Body: old + "\nSee [[STRAßE]].\n\n" + bullets(ns), Notes: ids(ns), BaseHash: Hash([]byte(old))}}})
	if rep.Pages[0].Status != Merged {
		t.Errorf("link [[STRAßE]] to Strasse not resolved: %+v", rep.Pages)
	}
}

// N04b: fences that are closed correctly, longer, or mixed in the old text pass; a merge that leaves one open does not.
func TestFenceRules(t *testing.T) {
	notes := []Note{{ID: "n1", Text: "The relay listens on port 9123 for the agents."}}
	old := "# P\n\nOld text of the page.\n"
	none := func(string) bool { return false }
	ok := map[string]string{
		"closed":           old + "\n```\nThe relay listens on port 9123 for the agents.\n```\n",
		"longer closer":    old + "\n```\nThe relay listens on port 9123 for the agents.\n````\n",
		"tilde in a fence": old + "\n````\n~~~\nThe relay listens on port 9123 for the agents.\n````\n",
	}
	for name, body := range ok {
		if probs := CheckPage(old, body, notes, none); len(probs) != 0 {
			t.Errorf("%s: %v", name, probs)
		}
	}
	bad := map[string]string{
		"unclosed":       old + "\n```\nThe relay listens on port 9123 for the agents.\n",
		"shorter closer": old + "\n````\nThe relay listens on port 9123 for the agents.\n```\n",
		"other char":     old + "\n~~~\nThe relay listens on port 9123 for the agents.\n```\n",
	}
	for name, body := range bad {
		if probs := CheckPage(old, body, notes, none); len(probs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	// a page that already ended inside a fence is old text
	oldOpen := old + "\n```\nstill open\n"
	if probs := CheckPage(oldOpen, oldOpen+"The relay listens on port 9123 for the agents.\n", notes, none); len(probs) != 0 {
		t.Errorf("old open fence blamed on the merge: %v", probs)
	}
}

// blockingEmbedder never answers until released or its context ends.
type blockingEmbedder struct {
	bag
	entered atomic.Int32
	release chan struct{}
}

func (b *blockingEmbedder) wait(ctx context.Context) error {
	b.entered.Add(1)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *blockingEmbedder) Docs(ctx context.Context, t []string) ([][]float32, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.bag.Docs(ctx, t)
}
func (b *blockingEmbedder) Query(ctx context.Context, t string) ([]float32, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.bag.Query(ctx, t)
}

// Crash window: Submit commits at once after the write. A process killed while the slow copy check runs (here: an
// embedder that never answers) leaves nothing uncommitted.
func TestSubmitCommitsBeforeTheSlowCopyCheck(t *testing.T) {
	st, repo := newStore(t)
	st.Commit.Stop()
	st.Commit = store.NewCommitter(repo, time.Hour) // no timer commit can hide the fix
	ns := learnN(t, st, proj, 2, tmplWorker)
	emb := &blockingEmbedder{release: make(chan struct{})}
	st.Emb = emb
	defer close(emb.release)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	}()
	time.Sleep(700 * time.Millisecond)
	select {
	case <-done:
		t.Skip("the embedder did not hold Submit; nothing to prove")
	default:
	}
	if out := git(t, repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		t.Errorf("DEFECT files uncommitted while the copy check runs:\n%s", out)
	}
	if !strings.Contains(git(t, repo, "log", "-1", "--format=%B"), "curate "+proj) {
		t.Errorf("the last commit is not the curate commit")
	}
}
