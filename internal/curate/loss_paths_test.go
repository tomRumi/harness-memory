package curate

// Data-loss paths. A failing test is a defect; its message starts with DEFECT.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/md"
	"github.com/tomRumi/harness-memory/internal/store"
)

// textSomewhere: the note file exists, or its whole flat text is in some wiki file of the repo.
func textSomewhere(t *testing.T, repo, project string, n Note) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(repo, "notes", project, n.ID+".md")); err == nil {
		return true
	}
	found := false
	filepath.Walk(filepath.Join(repo, "wiki"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			if strings.Contains(flat(string(b)), flat(n.Text)) {
				found = true
			}
		}
		return nil
	})
	return found
}

func gitStatus(repo string) string {
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain", "-uall").CombinedOutput()
	return string(out)
}

// L01 crash/failure after a page write and before the note delete: Submit returns an error half way.
func TestFailureAfterPageWriteLeavesUncommittedPage(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	commits(t, st, repo)
	dir := filepath.Join(repo, "notes", proj)
	os.Chmod(dir, 0o555) // the note delete fails
	defer os.Chmod(dir, 0o755)
	_, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: "# Workers\n\n" + bullets(ns), Notes: ids(ns)}}})
	os.Chmod(dir, 0o755)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !textSomewhere(t, repo, proj, ns[0]) {
		t.Errorf("DEFECT note text lost")
	}
	commits(t, st, repo)
	// a later serve start sweeps notes and facts only
	c := store.NewCommitter(repo, time.Millisecond)
	c.Sweep()
	c.Flush()
	if s := gitStatus(repo); strings.Contains(s, "wiki/") {
		t.Errorf("DEFECT half-applied submit: page written, never committed, not swept: %q (err was %v)", s, err)
	}
}

// L02 two submits in one process: serialised; the second sees a stale page.
func TestTwoSubmitsSameProcess(t *testing.T) {
	st, _ := newStore(t)
	old := "# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", old)
	ns := learnN(t, st, proj, 2, tmplWorker)
	h := Hash([]byte(old))
	var wg sync.WaitGroup
	reps := make([]Report, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reps[i], _ = Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Ports", Body: old + "\n" + bullets(ns[i:i+1]), Notes: []string{ns[i].ID}, BaseHash: h}}})
		}(i)
	}
	wg.Wait()
	for _, n := range ns {
		if !textSomewhere(t, st.Repo, proj, n) {
			t.Errorf("DEFECT note %s lost by concurrent submits", short(n.ID))
		}
	}
}

// L03 two submits from two processes (two Store values on one repository): the write lock is per process.
func TestTwoSubmitsTwoProcesses(t *testing.T) {
	lost := 0
	for round := 0; round < 30 && lost == 0; round++ {
		st, repo := newStore(t)
		old := "# Ports\n\nThe daemon uses a port.\n"
		putFile(t, st, "wiki/proj/Ports.md", old)
		ns := learnN(t, st, proj, 2, tmplWorker)
		db2, err := index.Open(filepath.Join(filepath.Dir(repo), "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		st2, err := store.New(repo, db2, &bag{}, store.NewCommitter(repo, 20*time.Millisecond), filepath.Join(filepath.Dir(repo), "events2.db"))
		if err != nil {
			t.Fatal(err)
		}
		h := Hash([]byte(old))
		var wg sync.WaitGroup
		for i, s := range []*store.Store{st, st2} {
			wg.Add(1)
			go func(i int, s *store.Store) {
				defer wg.Done()
				Submit(context.Background(), s, Submission{Project: proj, Pages: []Page{{Title: "Ports", Body: old + "\n" + bullets(ns[i:i+1]), Notes: []string{ns[i].ID}, BaseHash: h}}})
			}(i, s)
		}
		wg.Wait()
		for _, n := range ns {
			if !textSomewhere(t, repo, proj, n) {
				lost++
			}
		}
		st2.Close()
		db2.Close()
	}
	if lost > 0 {
		t.Errorf("DEFECT two processes submitting at once: a note was deleted and its text is in no page (%d)", lost)
	}
}

// L04 page hand-edited between pending and submit: stale, notes stay.
func TestHandEditBetweenPendingAndSubmit(t *testing.T) {
	st, _ := newStore(t)
	old := "# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", old)
	ns := learnN(t, st, proj, 1, tmplWorker)
	bs, _ := Pending(context.Background(), st, proj, 0)
	var h string
	for _, r := range bs[0].Related {
		if r.Title == "Ports" {
			h = r.Hash
		}
	}
	if h == "" {
		h = Hash([]byte(old))
	}
	os.WriteFile(filepath.Join(st.Repo, "wiki/proj/Ports.md"), []byte(old+"\nA hand edit.\n"), 0o644)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Ports", Body: old + "\n" + bullets(ns), Notes: ids(ns), BaseHash: h}}})
	if rep.Pages[0].Status != Stale || noteCount(t, st, proj) != 1 || !strings.Contains(readFile(t, st, "wiki/proj/Ports.md"), "A hand edit.") {
		t.Errorf("DEFECT hand edit: %+v", rep)
	}
}

// L05 a note assigned to two pages: nothing written.
func TestNoteOnTwoPages(t *testing.T) {
	st, _ := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	_, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "A", Body: bullets(ns), Notes: ids(ns)}, {Title: "B", Body: bullets(ns), Notes: []string{" " + ns[0].ID}}}})
	if err == nil || exists(st, "wiki/proj/A.md") || noteCount(t, st, proj) != 1 {
		t.Errorf("DEFECT double assignment: err=%v", err)
	}
}

// L06 ids from another project are unknown; that project's note stays.
func TestIdsOfAnotherProject(t *testing.T) {
	st, _ := newStore(t)
	other := learnN(t, st, "other", 1, tmplWorker)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "A", Body: bullets(other), Notes: ids(other)}}})
	if noteCount(t, st, "other") != 1 || len(rep.Unknown) != 1 {
		t.Errorf("DEFECT foreign id: %+v", rep)
	}
}

// L07 title tricks: nothing may be written outside wiki/proj, and the notes must survive.
func TestTitleTricks(t *testing.T) {
	for _, title := range []string{"..", "../x", "a/../../b", "sub/Page", `..\x`, ".hidden", "x\x00y", "Index", "unsorted", strings.Repeat("😀", 70), "Foo.md", "con"} {
		st, repo := newStore(t)
		ns := learnN(t, st, proj, 1, tmplWorker)
		rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: title, Summary: "s", Body: "# T\n\n" + bullets(ns), Notes: ids(ns)}}})
		var outside []string
		filepath.Walk(filepath.Dir(repo), func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".md") && !strings.Contains(p, "/wiki/proj/") && !strings.Contains(p, "/notes/") {
				outside = append(outside, p)
			}
			return nil
		})
		if len(outside) > 0 {
			t.Errorf("DEFECT title %q wrote outside wiki/proj: %v", title, outside)
		}
		if !textSomewhere(t, repo, proj, ns[0]) {
			t.Errorf("DEFECT title %q: note lost", title)
		}
		st2 := ""
		if len(rep.Pages) > 0 {
			st2 = rep.Pages[0].Status
		}
		if err != nil {
			t.Logf("title %q: error %v; git status %q", title, err, gitStatus(repo))
			if strings.Contains(gitStatus(repo), "wiki/") {
				t.Errorf("DEFECT title %q: Submit failed half way and left uncommitted files: %q", title, gitStatus(repo))
			}
		} else {
			t.Logf("title %q: %s", title, st2)
		}
	}
}

// L08 summary and tags with newline, ---, YAML syntax: new page and existing page must still parse.
func TestSummaryAndTagsYAML(t *testing.T) {
	bad := []string{"a\nb", "---", "key: value", "- item", "[x, y]", "{a: 1}", "#comment", "'q'", "\"dq\"", "a\n---\nb: c", "yes", "null", "\\x", "\u2028sep", "\t tab", "&anchor", "*alias", "!tag", "|", ">"}
	for i, s := range bad {
		st, _ := newStore(t)
		old := "---\ntitle: Ports\nsummary: old\ntags: [net]\n---\n\n# Ports\n\nThe daemon uses a port.\n"
		putFile(t, st, "wiki/proj/Ports.md", old)
		ns := learnN(t, st, proj, 2, tmplWorker)
		rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
			{Title: "Ports", Summary: s, Tags: []string{s, "ok"}, Body: "# Ports\n\nThe daemon uses a port.\n\n" + bullets(ns[:1]), Notes: ids(ns[:1]), BaseHash: Hash([]byte(old))},
			{Title: fmt.Sprintf("New %d", i), Summary: s, Tags: []string{s}, Body: "# New\n\n" + bullets(ns[1:]), Notes: ids(ns[1:])},
		}})
		if err != nil {
			t.Errorf("DEFECT %q: %v", s, err)
			continue
		}
		for _, f := range []string{"wiki/proj/Ports.md", fmt.Sprintf("wiki/proj/New %d.md", i)} {
			if !exists(st, f) {
				t.Logf("%q: %s not written (%+v)", s, f, rep.Pages)
				continue
			}
			d := md.Parse([]byte(readFile(t, st, f)))
			if d.Error != "" || d.Str("title") == "" {
				t.Errorf("DEFECT summary/tag %q broke the frontmatter of %s: %s\n%s", s, f, d.Error, readFile(t, st, f))
			}
		}
	}
}

// L09 a body that starts with --- (frontmatter injection) on a new page and on an existing page without frontmatter.
func TestBodyStartsWithDashes(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "wiki/proj/Plain.md", "# Plain\n\nOld text.\n")
	ns := learnN(t, st, proj, 2, tmplWorker)
	evil := "---\ntitle: Evil\nsummary: injected\n---\n"
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Fresh", Summary: "s", Body: evil + "# Fresh\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "Plain", Body: evil + "# Plain\n\nOld text.\n\n" + bullets(ns[1:]), Notes: ids(ns[1:]), BaseHash: Hash([]byte("# Plain\n\nOld text.\n"))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"wiki/proj/Fresh.md", "wiki/proj/Plain.md"} {
		if !exists(st, f) {
			t.Logf("%s not written: %+v", f, rep.Pages)
			continue
		}
		d := md.Parse([]byte(readFile(t, st, f)))
		if d.Str("title") == "Evil" || d.Error != "" {
			t.Errorf("DEFECT body frontmatter took over %s: title=%q err=%q\n%s", f, d.Str("title"), d.Error, readFile(t, st, f))
		}
	}
}

// L10 a huge body: refused, notes to Unsorted, nothing lost.
func TestHugeBody(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	body := bullets(ns) + strings.Repeat("filler words here and there\n", 400000)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Huge", Body: body, Notes: ids(ns)}}})
	if err != nil || !textSomewhere(t, repo, proj, ns[0]) || exists(st, "wiki/proj/Huge.md") {
		t.Errorf("DEFECT huge body: %v %+v", err, rep.Pages)
	}
}

// L11 a note file whose id is not a uuid: pending hands it out, submit never takes it (stuck for ever).
func TestNonUUIDNoteID(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "notes/proj/my note.md", "---\nkind: learning\n---\n\nThe worker restarts after five failed health checks and pages the owner.\n")
	bs, _ := Pending(context.Background(), st, proj, 0)
	if len(bs) == 0 || len(bs[0].Notes) == 0 {
		t.Skip("not handed out")
	}
	n := bs[0].Notes[0]
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Body: "# W\n\n- " + n.Text + "\n", Notes: []string{n.ID}}}})
	if len(rep.Unknown) == 1 && noteCount(t, st, proj) == 1 {
		t.Errorf("DEFECT note id %q is handed out by -pending but -submit calls it unknown: it never leaves (design: nothing gets stuck); pages=%+v", n.ID, rep.Pages)
	}
	if exists(st, "wiki/proj/Workers.md") && len(rep.Unknown) == 1 {
		t.Logf("page written without the note")
	}
}

// L12 an existing page whose tags/sources are a YAML block list without indent (valid YAML, the form many tools write).
func TestBlockListFrontmatter(t *testing.T) {
	for _, old := range []string{
		"---\ntitle: Ports\ntags:\n- net\n- ops\nsources:\n- 2026-10-01: abc\n---\n\n# Ports\n\nThe daemon uses a port.\n",
		"---\ntitle: Ports\ntags:\n  - net\nsources:\n  - \"2026-10-01: abc\"\n---\n\n# Ports\n\nThe daemon uses a port.\n",
		"---\ntitle: Ports\nsummary: >\n  folded\n  summary\n---\n\n# Ports\n\nThe daemon uses a port.\n",
	} {
		st, _ := newStore(t)
		putFile(t, st, "wiki/proj/Ports.md", old)
		ns := learnN(t, st, proj, 1, tmplWorker)
		rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Ports", Summary: "new summary", Tags: []string{"extra"}, Body: "# Ports\n\nThe daemon uses a port.\n\n" + bullets(ns), Notes: ids(ns), BaseHash: Hash([]byte(old))}}})
		if err != nil {
			t.Fatal(err)
		}
		got := readFile(t, st, "wiki/proj/Ports.md")
		if d := md.Parse([]byte(got)); d.Error != "" {
			t.Errorf("DEFECT the merge broke the page's frontmatter (%s); status %s\nbefore:\n%s\nafter:\n%s", d.Error, rep.Pages[0].Status, old, got)
		}
	}
}

// L13 a note deleted by rating retirement during a submit: its text is in no page and not in Unsorted.
func TestRatingRetirementDeletesUnplacedNote(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 2, tmplWorker)
	for i := 0; i < store.RetireAfter; i++ {
		st.Feedback(ns[1].ID, false, proj, fmt.Sprintf("s%d", i))
	}
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Body: "# W\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])}}})
	if !textSomewhere(t, repo, proj, ns[1]) {
		t.Errorf("DEFECT (not in design v6) a note not in the submission was deleted by ratings, text in no page/Unsorted: merged=%d", rep.Merged)
	}
}

// L14 a hand-written Index.md is overwritten by the generated index.
func TestHandWrittenIndexOverwritten(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "wiki/proj/Index.md", "# My own index\n\nHand-written overview of the whole project, years of notes.\n")
	ns := learnN(t, st, proj, 1, tmplWorker)
	Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	if !strings.Contains(readFile(t, st, "wiki/proj/Index.md"), "years of notes") {
		t.Errorf("DEFECT a hand-written Index.md was overwritten without a check")
	}
}

// L15 a note made of punctuation only: CheckPage finds it everywhere, the note is deleted with no text placed.
func TestPunctuationOnlyNote(t *testing.T) {
	st, repo := newStore(t)
	// learn refuses it (an earlier expectation was that it was stored); a note file that came by other ways is the case here
	if _, err := st.Learn(context.Background(), "=> !!! ??? -> ...", proj, "", ""); err == nil || !strings.Contains(err.Error(), "nothing to remember") {
		t.Errorf("learn must refuse a note with no letter or digit: %v", err)
	}
	id := "punct-only"
	putFile(t, st, "notes/proj/"+id+".md", "---\nkind: learning\n---\n\n=> !!! ??? -> ...\n")
	n := Note{ID: id, Text: "=> !!! ??? -> ..."}
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Empty", Body: "# Nothing here\n", Notes: []string{id}}}})
	if rep.Pages[0].Status == Merged {
		t.Errorf("a note with no letter or digit counts as found: %+v", rep.Pages)
	}
	if !textSomewhere(t, repo, proj, n) {
		t.Errorf("DEFECT a note with no letters passes the check without being in the page: %+v", rep.Pages)
	}
}

// L16 Unicode normalisation: an NFC title over an NFD file name (macOS file systems treat them as one file).
func TestNFCTitleOverwritesNFDPage(t *testing.T) {
	st, repo := newStore(t)
	nfd := "Cafe\u0301"
	nfc := "Caf\u00e9"
	old := "# Café\n\nYears of hand-written text about the cafe machine.\n"
	putFile(t, st, "wiki/proj/"+nfd+".md", old)
	ns := learnN(t, st, proj, 1, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: nfc, Summary: "s", Body: "# Café\n\n" + bullets(ns), Notes: ids(ns)}}})
	ents, _ := os.ReadDir(filepath.Join(repo, "wiki", proj))
	all := ""
	for _, e := range ents {
		if e.Name() == "Index.md" {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(repo, "wiki", proj, e.Name()))
		all += string(b)
	}
	t.Logf("files %v err %v rep %+v\n%s", len(ents), err, rep.Pages, all)
	if !strings.Contains(all, "Years of hand-written text") {
		t.Errorf("DEFECT an NFC title overwrote the NFD page with no base_hash check: %v %+v", err, rep.Pages)
	}
}

// L17 two new pages in one submission whose titles differ only in normalisation: one file, one overwrites the other.
func TestTwoTitlesSameFileInOneSubmission(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 2, tmplWorker)
	Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Cafe\u0301", Summary: "s", Body: "# C\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "Caf\u00e9", Summary: "s", Body: "# C\n\n" + bullets(ns[1:]), Notes: ids(ns[1:])},
	}})
	for _, n := range ns {
		if !textSomewhere(t, repo, proj, n) {
			t.Errorf("DEFECT note %s deleted; its page was overwritten by a page of the same submission", short(n.ID))
		}
	}
}

// L18 project name with capitals through Submit (the CLI takes -p as given).
func TestProjectCase(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	commits(t, st, repo)
	rep, err := Submit(context.Background(), st, Submission{Project: "PROJ", Pages: []Page{{Title: "Workers", Summary: "s", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	commits(t, st, repo)
	tree := git(t, repo, "ls-files")
	t.Logf("err=%v merged=%d unknown=%v\ntree:\n%s\nstatus:%s", err, rep.Merged, rep.Unknown, tree, gitStatus(repo))
	if rep.Merged > 0 && strings.Contains(tree, "wiki/PROJ/") {
		t.Errorf("DEFECT project %q accepted: git now holds wiki/PROJ next to wiki/proj (two folders on a case-sensitive clone)", "PROJ")
	}
	if strings.TrimSpace(gitStatus(repo)) != "" {
		t.Errorf("DEFECT project case: uncommitted changes left: %q", gitStatus(repo))
	}
}

// L19 a commit failure after the files changed: the files stay on disk and the commit happens once git works.
func TestCommitFailureThenRecovery(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	commits(t, st, repo)
	os.WriteFile(filepath.Join(repo, "foreign.txt"), []byte("x"), 0o644)
	git(t, repo, "add", "foreign.txt")
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil || rep.Merged != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	st.Commit.Flush()
	if st.Commit.LastErr() == nil {
		t.Errorf("expected a commit error with a foreign file staged")
	}
	git(t, repo, "reset", "-q", "foreign.txt")
	st.Commit.Flush()
	if s := gitStatus(repo); strings.Contains(s, "wiki/") || strings.Contains(s, "notes/") {
		t.Errorf("DEFECT after recovery, uncommitted: %q", s)
	}
}

// L20 a page that passes the check but whose frontmatter cannot be edited: the composePage error path. A page linking
// to it was already accepted; the link is now broken.
func TestComposeFailureLeavesBrokenLink(t *testing.T) {
	st, _ := newStore(t)
	old := "---\ntitle: Ports\n---\n\n# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", old)
	ns := learnN(t, st, proj, 1, tmplWorker)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Ports", Body: "# Ports\n\nThe daemon uses a port.\n\n" + bullets(ns), Notes: ids(ns), BaseHash: Hash([]byte(old))}}})
	if rep.Merged != 1 {
		t.Errorf("DEFECT simple update not merged: %+v", rep.Pages)
	}
}
