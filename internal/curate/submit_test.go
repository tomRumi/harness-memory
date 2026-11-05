package curate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory-service/internal/store"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

func TestSubmitHappyPathTwelveNotesIntoTwoPagesOneCommit(t *testing.T) {
	st, repo := newStore(t)
	ctx := context.Background()
	oldPorts := "---\ntitle: Ports\nsummary: Ports used\ntags: [network]\nsources: [\"2026-10-01: abc12345\"]\ncustom: keep me # a comment\n---\n\n# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", oldPorts)
	notes := learnN(t, st, proj, 12, tmplWorker)
	before := commits(t, st, repo)
	portsBase := Hash([]byte(oldPorts))
	rep, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{
		{Title: "Ports", Summary: "Ports and the workers", Tags: []string{"network", "ops"},
			Body: "# Ports\n\nThe daemon uses a port.\n\n" + bullets(notes[:6]), Notes: ids(notes[:6]), BaseHash: portsBase},
		{Title: "Backups", Summary: "How the workers are watched", Tags: []string{"ops"},
			Body: "# Backups\n\nSee [[Ports]].\n\n" + bullets(notes[6:]), Notes: ids(notes[6:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 12 || rep.ToUnsorted != 0 || len(rep.Left) != 0 || len(rep.Unknown) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	for _, p := range rep.Pages {
		if p.Status != Merged || len(p.Problems) != 0 {
			t.Fatalf("page %+v", p)
		}
	}
	if rep.Commit != "curate proj: 12 notes into 2 pages (0 to Unsorted)" {
		t.Fatalf("commit message %q", rep.Commit)
	}
	if n := noteCount(t, st, proj); n != 0 {
		t.Fatalf("%d notes left", n)
	}

	// the existing page keeps its frontmatter byte for byte except summary, tags and sources
	got := readFile(t, st, "wiki/proj/Ports.md")
	for _, want := range []string{"title: Ports\n", "custom: keep me # a comment\n", `summary: "Ports and the workers"`, `tags: ["network","ops"]`} {
		if !strings.Contains(got, want) {
			t.Fatalf("Ports.md lacks %q:\n%s", want, got)
		}
	}
	d := fmOf(t, got)
	m := wiki.ParseMeta(d, "Ports")
	if len(m.Sources) != 2 || m.Sources[0] != "2026-10-01: abc12345" || !strings.HasPrefix(m.Sources[1], "2026-11-05: "+notes[0].ID[:8]+" ") {
		t.Fatalf("sources %q", m.Sources)
	}
	if strings.Count(m.Sources[1], " ") != 6 { // the date and six short ids
		t.Fatalf("sources entry %q", m.Sources[1])
	}
	if !strings.Contains(d.Body, "The daemon uses a port.") || !strings.Contains(d.Body, notes[5].Text) {
		t.Fatalf("body:\n%s", d.Body)
	}

	// the new page gets a frontmatter composed by the service
	nd := fmOf(t, readFile(t, st, "wiki/proj/Backups.md"))
	nm := wiki.ParseMeta(nd, "Backups")
	if nm.Title != "Backups" || nm.Summary != "How the workers are watched" || len(nm.Tags) != 1 || nm.Tags[0] != "ops" || len(nm.Sources) != 1 || !strings.HasPrefix(nm.Sources[0], "2026-11-05: ") {
		t.Fatalf("meta %+v", nm)
	}

	// the Index page is regenerated and lists both pages
	idx := readFile(t, st, "wiki/proj/Index.md")
	if !strings.Contains(idx, "- [[Backups]] - How the workers are watched\n") || !strings.Contains(idx, "- [[Ports]] - Ports and the workers\n") {
		t.Fatalf("Index.md:\n%s", idx)
	}
	// the index finds the new links
	if broken, amb, _ := st.DB.LinkCounts(); broken != 0 || amb != 0 {
		t.Fatalf("links: %d broken, %d ambiguous", broken, amb)
	}

	// exactly one commit, with the deletions in it
	if n := commits(t, st, repo); n != before+1 {
		t.Fatalf("%d commits after the submit, want %d", n, before+1)
	}
	if subj := strings.TrimSpace(git(t, repo, "log", "-1", "--format=%s")); subj != rep.Commit {
		t.Fatalf("subject %q", subj)
	}
	if body := git(t, repo, "log", "-1", "--format=%b"); !strings.Contains(body, notes[0].ID) || !strings.Contains(body, notes[11].ID) {
		t.Fatalf("the message lacks the note ids:\n%s", body)
	}
	stat := git(t, repo, "log", "--stat", "-1")
	if strings.Count(stat, "notes/proj/") != 12 || !strings.Contains(stat, "12 deletions") && !strings.Contains(stat, "deletions(-)") {
		t.Fatalf("git log --stat:\n%s", stat)
	}
	ns := git(t, repo, "show", "--name-status", "--format=", "HEAD")
	if strings.Count(ns, "D\tnotes/proj/") != 12 || !strings.Contains(ns, "M\twiki/proj/Ports.md") || !strings.Contains(ns, "A\twiki/proj/Backups.md") || !strings.Contains(ns, "A\twiki/proj/Index.md") {
		t.Fatalf("name-status:\n%s", ns)
	}
	if git(t, repo, "status", "--porcelain") != "" {
		t.Fatalf("uncommitted files:\n%s", git(t, repo, "status", "--porcelain"))
	}
}

func TestSubmitFailingPageGoesToUnsortedOthersStillWritten(t *testing.T) {
	st, repo := newStore(t)
	notes := learnN(t, st, proj, 8, tmplWorker)
	commits(t, st, repo)
	lossy := strings.Replace(bullets(notes[:4]), "after 10 failed", "after 99 failed", 1)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Lossy", Summary: "x", Body: "# Lossy\n\n" + lossy, Notes: ids(notes[:4])},
		{Title: "Good", Summary: "y", Body: "# Good\n\n" + bullets(notes[4:]), Notes: ids(notes[4:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 4 || rep.ToUnsorted != 4 || rep.Pages[0].Status != Unsorted || rep.Pages[1].Status != Merged {
		t.Fatalf("%+v", rep)
	}
	if len(rep.Pages[0].Problems) == 0 || !strings.Contains(rep.Pages[0].Problems[0], "number 10") {
		t.Fatalf("problems %v", rep.Pages[0].Problems)
	}
	if exists(st, "wiki/proj/Lossy.md") || !exists(st, "wiki/proj/Good.md") {
		t.Fatal("Lossy must not be written, Good must")
	}
	un := readFile(t, st, "wiki/proj/Unsorted.md")
	d := fmOf(t, un)
	if d.Str("title") != "Unsorted" || d.Str("summary") != UnsortedSummary {
		t.Fatalf("Unsorted frontmatter %v", d.FM)
	}
	for _, n := range notes[:4] {
		want := fmt.Sprintf("- %s (2026-11-05, %s)\n", n.Text, n.ID[:8])
		if !strings.Contains(un, want) {
			t.Fatalf("Unsorted lacks %q:\n%s", want, un)
		}
	}
	if noteCount(t, st, proj) != 0 {
		t.Fatal("all eight notes should be gone")
	}
	if rep.Commit != "curate proj: 4 notes into 1 pages (4 to Unsorted)" {
		t.Fatal(rep.Commit)
	}
	if !strings.Contains(readFile(t, st, "wiki/proj/Index.md"), "[[Unsorted]]") {
		t.Fatal("the index should list Unsorted")
	}
	if !strings.Contains(rep.String(), "unsorted") || !strings.Contains(rep.String(), "number 10") {
		t.Fatalf("text report:\n%s", rep)
	}
}

func TestSubmitUnsortedCreatedThenAppendedLaterKeepingOldLines(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	first := learnN(t, st, proj, 2, tmplWorker)
	if _, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{{Title: "Bad", Body: "nothing useful here", Notes: ids(first)}}}); err != nil {
		t.Fatal(err)
	}
	un1 := readFile(t, st, "wiki/proj/Unsorted.md")
	// a person edits the page, then a second merge fails too
	un1 = strings.TrimRight(un1, "\n") + "\n\nA line I added by hand."
	putFile(t, st, "wiki/proj/Unsorted.md", un1)
	second, _ := st.Learn(ctx, "Another observation that is long enough to be a real note.", proj, "learning", "")
	if _, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{{Title: "Bad2", Body: "still nothing", Notes: []string{second}}}}); err != nil {
		t.Fatal(err)
	}
	un2 := readFile(t, st, "wiki/proj/Unsorted.md")
	if !strings.HasPrefix(un2, un1+"\n") {
		t.Fatalf("old lines were touched:\n%s\n---\n%s", un1, un2)
	}
	if !strings.HasSuffix(un2, "- Another observation that is long enough to be a real note. (2026-11-05, "+second[:8]+")\n") {
		t.Fatalf("appended bullet:\n%s", un2)
	}
}

func TestSubmitStalePagesKeepTheirNotesPending(t *testing.T) {
	st, repo := newStore(t)
	ctx := context.Background()
	old := "# Ports\n\nThe daemon uses a port.\n"
	putFile(t, st, "wiki/proj/Ports.md", old)
	putFile(t, st, "wiki/proj/Existing.md", "# Existing\n\nAlready here.\n")
	base := Hash([]byte(old))
	notes := learnN(t, st, proj, 6, tmplWorker)
	commits(t, st, repo)
	// a hand edit while the job was working
	edited := old + "Edited by hand meanwhile.\n"
	putFile(t, st, "wiki/proj/Ports.md", edited)
	rep, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{
		{Title: "Ports", Body: old + bullets(notes[:2]), Notes: ids(notes[:2]), BaseHash: base},
		{Title: "existing", Body: "# New\n\n" + bullets(notes[2:4]), Notes: ids(notes[2:4])}, // a new title that exists (case differs): the page is not overwritten
		{Title: "Fresh", Summary: "z", Body: "# Fresh\n\n" + bullets(notes[4:]), Notes: ids(notes[4:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages[0].Status != Stale || rep.Pages[1].Status != Stale || rep.Pages[2].Status != Merged || !strings.Contains(rep.Pages[1].Problems[0], "already exists") {
		t.Fatalf("%+v", rep.Pages)
	}
	if readFile(t, st, "wiki/proj/Ports.md") != edited {
		t.Fatal("the hand-edited page was overwritten")
	}
	if rep.Merged != 2 || len(rep.Left) != 4 || noteCount(t, st, proj) != 4 {
		t.Fatalf("merged %d, left %v, pending %d", rep.Merged, rep.Left, noteCount(t, st, proj))
	}
	if rep.ToUnsorted != 0 || exists(st, "wiki/proj/Unsorted.md") {
		t.Fatal("a stale page must not feed Unsorted")
	}
}

func TestSubmitBaseHashOfAPageThatIsGoneIsStale(t *testing.T) {
	st, _ := newStore(t)
	notes := learnN(t, st, proj, 1, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Gone", Body: bullets(notes), Notes: ids(notes), BaseHash: "abc"}}})
	if err != nil || rep.Pages[0].Status != Stale || noteCount(t, st, proj) != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestSubmitUnassignedNotesAreLeftAndUnknownIdsListed(t *testing.T) {
	st, _ := newStore(t)
	notes := learnN(t, st, proj, 5, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Some", Summary: "s", Body: bullets(notes[:2]), Notes: append(ids(notes[:2]), "no-such-id", "../etc")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 2 || len(rep.Left) != 3 || len(rep.Unknown) != 2 || rep.Unknown[0] != "no-such-id" {
		t.Fatalf("%+v", rep)
	}
	if noteCount(t, st, proj) != 3 {
		t.Fatal("three notes should stay")
	}
	if !strings.Contains(rep.String(), "left:") || !strings.Contains(rep.String(), "unknown: no-such-id") {
		t.Fatal(rep.String())
	}
}

func TestSubmitDuplicateAssignmentRejectsEverything(t *testing.T) {
	st, repo := newStore(t)
	notes := learnN(t, st, proj, 3, tmplWorker)
	before := commits(t, st, repo)
	_, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "A", Body: bullets(notes[:2]), Notes: ids(notes[:2])},
		{Title: "B", Body: bullets(notes[1:]), Notes: ids(notes[1:])},
	}})
	if err == nil || !strings.Contains(err.Error(), "two pages") {
		t.Fatalf("err %v", err)
	}
	if noteCount(t, st, proj) != 3 || exists(st, "wiki/proj/A.md") || exists(st, "wiki/proj/B.md") || exists(st, "wiki/proj/Index.md") || commits(t, st, repo) != before {
		t.Fatal("something was written")
	}
	// the same id twice inside one page is not a conflict
	if _, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "A", Summary: "a", Body: bullets(notes[:1]), Notes: []string{notes[0].ID, notes[0].ID}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitRefusedWhileMergeInProgress(t *testing.T) {
	st, repo := newStore(t)
	notes := learnN(t, st, proj, 2, tmplWorker)
	before := commits(t, st, repo)
	if err := os.WriteFile(filepath.Join(repo, ".git", "MERGE_HEAD"), []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "A", Body: bullets(notes), Notes: ids(notes)}}})
	if err == nil || !strings.Contains(err.Error(), "merge or rebase") {
		t.Fatalf("err %v", err)
	}
	if noteCount(t, st, proj) != 2 || exists(st, "wiki/proj/A.md") {
		t.Fatal("something was written")
	}
	os.Remove(filepath.Join(repo, ".git", "MERGE_HEAD"))
	if commits(t, st, repo) != before {
		t.Fatal("a commit was made")
	}
}

func TestSubmitTitleRules(t *testing.T) {
	st, _ := newStore(t)
	notes := learnN(t, st, proj, 5, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "a/b", Body: bullets(notes[:1]), Notes: ids(notes[:1])},
		{Title: "Index", Body: bullets(notes[1:2]), Notes: ids(notes[1:2])},
		{Title: "unsorted", Body: bullets(notes[2:3]), Notes: ids(notes[2:3])},
		{Title: ".hidden", Body: bullets(notes[3:4]), Notes: ids(notes[3:4])},
		{Title: "Fine", Summary: "f", Body: bullets(notes[4:]), Notes: ids(notes[4:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if rep.Pages[i].Status != Refused || len(rep.Pages[i].Problems) != 1 {
			t.Fatalf("page %d: %+v", i, rep.Pages[i])
		}
	}
	if !strings.Contains(rep.String(), "refused(a/b)") {
		t.Fatal(rep.String())
	}
	if rep.ToUnsorted != 4 || rep.Merged != 1 {
		t.Fatalf("%+v", rep)
	}
	// the refused notes are bullets in Unsorted; no page took a forbidden name
	if !exists(st, "wiki/proj/Unsorted.md") || exists(st, "wiki/proj/a/b.md") || !exists(st, "wiki/proj/Fine.md") {
		t.Fatal("files")
	}
	if got := readFile(t, st, "wiki/proj/Index.md"); strings.Contains(got, "[[Index]]") {
		t.Fatal(got)
	}
}

func TestSubmitLinkToAPageCreatedInTheSameSubmissionButFailingIsBroken(t *testing.T) {
	st, _ := newStore(t)
	notes := learnN(t, st, proj, 4, tmplWorker)
	// A links to B and B links to A: both fine
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "A", Summary: "a", Body: "See [[B]].\n" + bullets(notes[:2]), Notes: ids(notes[:2])},
		{Title: "B", Summary: "b", Body: "See [[a]].\n" + bullets(notes[2:]), Notes: ids(notes[2:])},
	}})
	if err != nil || rep.Merged != 4 {
		t.Fatalf("%v %+v", err, rep)
	}
	// C is lossy and D links to C: C goes to Unsorted, so D's link would be broken and D follows
	more := learnN(t, st, proj, 6, "Other %d fact says the pool grows to %d slots when the queue is full.")
	rep, err = Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "C", Body: "nothing", Notes: ids(more[:3])},
		{Title: "D", Summary: "d", Body: "See [[C]].\n" + bullets(more[3:]), Notes: ids(more[3:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages[0].Status != Unsorted || rep.Pages[1].Status != Unsorted || !strings.Contains(strings.Join(rep.Pages[1].Problems, ";"), "broken link [[C]]") {
		t.Fatalf("%+v", rep.Pages)
	}
	if exists(st, "wiki/proj/C.md") || exists(st, "wiki/proj/D.md") {
		t.Fatal("neither page should be written")
	}
}

func TestSubmitAccentedTitleAndLinkToIt(t *testing.T) {
	st, _ := newStore(t)
	notes := learnN(t, st, proj, 4, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Őrzés és café", Summary: "Accents", Body: bullets(notes[:2]), Notes: ids(notes[:2])},
		{Title: "Plain", Summary: "p", Body: "Back to [[őrzés ÉS café]].\n" + bullets(notes[2:]), Notes: ids(notes[2:])},
	}})
	if err != nil || rep.Merged != 4 {
		t.Fatalf("%v %+v", err, rep)
	}
	if !exists(st, "wiki/proj/Őrzés és café.md") {
		t.Fatal("file missing")
	}
	if !strings.Contains(readFile(t, st, "wiki/proj/Index.md"), "- [[Őrzés és café]] - Accents\n") {
		t.Fatal(readFile(t, st, "wiki/proj/Index.md"))
	}
	if b, a, _ := st.DB.LinkCounts(); b != 0 || a != 0 {
		t.Fatal("links")
	}
}

// Ratings only list candidates (hms curate shows them, retire deletes): a merge never deletes a note by itself.
func TestSubmitRatingsDoNotDeleteNotes(t *testing.T) {
	st, repo := newStore(t)
	notes := learnN(t, st, proj, 4, tmplWorker)
	before := commits(t, st, repo)
	for i := 0; i < store.RetireAfter; i++ {
		if _, err := st.Feedback(notes[3].ID[:8], false, proj, fmt.Sprintf("s%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if c := st.RetireCandidates(); len(c) != 1 || c[0] != "notes/proj/"+notes[3].ID {
		t.Fatalf("candidates %v", c)
	}
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Some", Summary: "s", Body: bullets(notes[:2]), Notes: ids(notes[:2])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Left) != 2 || rep.Left[0] != notes[2].ID || rep.Left[1] != notes[3].ID {
		t.Fatalf("left %v", rep.Left)
	}
	if !exists(st, "notes/proj/"+notes[3].ID+".md") || !exists(st, "notes/proj/"+notes[2].ID+".md") {
		t.Fatal("a rated note was deleted")
	}
	if commits(t, st, repo) != before+1 {
		t.Fatal("not one commit")
	}
	if ns := git(t, repo, "show", "--name-status", "--format=", "HEAD"); strings.Contains(ns, "D\tnotes/proj/"+notes[3].ID+".md") {
		t.Fatal(ns)
	}
}

func TestSubmitRecordsACurateEventAndNothingChangedMakesNoCommit(t *testing.T) {
	st, repo := newStore(t)
	notes := learnN(t, st, proj, 2, tmplWorker)
	before := commits(t, st, repo)
	// everything stale: nothing is written, nothing is committed
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "X", Body: "x", Notes: ids(notes), BaseHash: "deadbeef"}}})
	if err != nil || len(rep.Left) != 2 {
		t.Fatalf("%v %+v", err, rep)
	}
	if commits(t, st, repo) != before || exists(st, "wiki/proj/Index.md") {
		t.Fatal("a stale submit must change nothing")
	}
}

func TestSubmitInvalidProject(t *testing.T) {
	st, _ := newStore(t)
	for _, p := range []string{"", "a/b", ".x"} {
		if _, err := Submit(context.Background(), st, Submission{Project: p}); err == nil {
			t.Fatalf("project %q accepted", p)
		}
	}
}

func TestSubmitSurvivesEmbedderDown(t *testing.T) {
	st, repo := newStore(t)
	notes := learnN(t, st, proj, 2, tmplWorker)
	st.Emb.(*bag).fail.Store(true)
	before := commits(t, st, repo)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Some", Summary: "s", Body: bullets(notes), Notes: ids(notes)}}})
	if err != nil || rep.Merged != 2 || commits(t, st, repo) != before+1 {
		t.Fatalf("%v %+v", err, rep)
	}
	if st.EmbedError() == nil {
		t.Fatal("the embedding error should be kept")
	}
}
