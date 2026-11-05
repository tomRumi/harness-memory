package curate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Item 5: the project rule lower-cases (PROJ means proj); no wiki/PROJ is ever made.
func TestUpperCaseProjectIsLowerCasedByPendingAndSubmit(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	if bs, err := Pending(context.Background(), st, "PROJ", 0); err != nil || len(bs) != 1 || bs[0].Project != proj {
		t.Fatalf("pending: %v %+v", err, bs)
	}
	rep, err := Submit(context.Background(), st, Submission{Project: "PROJ", Pages: []Page{{Title: "W", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil || rep.Project != proj || rep.Merged != 1 {
		t.Fatalf("submit: %v %+v", err, rep)
	}
	for _, d := range []string{"wiki", "notes"} { // exact names: Stat("PROJ") succeeds for proj on a case-insensitive file system
		ents, _ := os.ReadDir(filepath.Join(repo, d))
		for _, e := range ents {
			if e.Name() == "PROJ" {
				t.Fatalf("%s/PROJ was created", d)
			}
		}
	}
	if !exists(st, "wiki/proj/W.md") {
		t.Fatal("the page is not under wiki/proj")
	}
	if _, err := Pending(context.Background(), st, "../x", 0); err == nil {
		t.Fatal("path accepted")
	}
}

// Item 6/3: titles over 200 bytes go to Unsorted, the text is kept.
func TestTitleOver200BytesGoesToUnsorted(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	title := strings.Repeat("語", 67) // 67 characters, 201 bytes
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: title, Body: bullets(ns), Notes: ids(ns)}}})
	if err != nil || rep.Pages[0].Status != Refused || !strings.Contains(strings.Join(rep.Pages[0].Problems, " "), "bytes") {
		t.Fatalf("%v %+v", err, rep.Pages)
	}
	if !textSomewhere(t, repo, proj, ns[0]) || !exists(st, "wiki/proj/Unsorted.md") {
		t.Fatal("note lost")
	}
}

// Item 3: two titles that normalise equal are duplicates; the second one's notes go to Unsorted.
func TestNormalisedEqualTitlesAreDuplicates(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 2, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "Cafe\u0301", Summary: "s", Body: "# C\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "Caf\u00e9", Summary: "s", Body: "# C\n\n" + bullets(ns[1:]), Notes: ids(ns[1:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages[0].Status != Merged || rep.Pages[1].Status != Refused || rep.ToUnsorted != 1 {
		t.Fatalf("%+v", rep)
	}
	for _, n := range ns {
		if !textSomewhere(t, repo, proj, n) {
			t.Fatalf("note %s lost", short(n.ID))
		}
	}
}

// Item 3: an NFC title over an existing NFD page is the same page: stale without the base_hash, and with it the
// page is changed in place, not a second file.
func TestNFCTitleIsTheSamePageAsTheNFDFile(t *testing.T) {
	st, repo := newStore(t)
	old := "# Café\n\nThe existing text about the cafe machine.\n"
	putFile(t, st, "wiki/proj/Cafe\u0301.md", old)
	ns := learnN(t, st, proj, 1, tmplWorker)
	rep, _ := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Caf\u00e9", Body: old + "\n" + bullets(ns), Notes: ids(ns)}}})
	if rep.Pages[0].Status != Stale || noteCount(t, st, proj) != 1 {
		t.Fatalf("%+v", rep.Pages)
	}
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Caf\u00e9", Body: old + "\n" + bullets(ns), Notes: ids(ns), BaseHash: Hash([]byte(old))}}})
	if err != nil || rep.Pages[0].Status != Merged {
		t.Fatalf("%v %+v", err, rep.Pages)
	}
	ents, _ := os.ReadDir(filepath.Join(repo, "wiki", proj))
	n := 0
	for _, e := range ents {
		if e.Name() != "Index.md" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d page files", n)
	}
}

// Item 3: link resolution matches NFC-normalised names (index and check).
func TestLinksResolveAcrossNormalisation(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "wiki/proj/Cafe\u0301.md", "# Café\n\nThe existing text about the cafe machine.\n")
	st.DB.Rescan(st.Repo)
	ok, err := st.DB.LinkResolver(proj)
	if err != nil {
		t.Fatal(err)
	}
	if !ok("Caf\u00e9") || !ok("CAFE\u0301") || ok("Cafes") {
		t.Fatal("NFC/NFD link resolution")
	}
	if id, _, _ := st.DB.ResolveTitle("Caf\u00e9"); id == "" {
		t.Fatal("ResolveTitle")
	}
	ns := learnN(t, st, proj, 1, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Linker", Summary: "s", Body: "# L\n\nSee [[Caf\u00e9]].\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil || rep.Pages[0].Status != Merged {
		t.Fatalf("%v %+v", err, rep.Pages)
	}
}

// Item 13: a hand-written Index.md stays, and the report says so.
func TestSubmitReportsAHandWrittenIndex(t *testing.T) {
	st, _ := newStore(t)
	putFile(t, st, "wiki/proj/Index.md", "# My own index\n\nHand-written overview.\n")
	ns := learnN(t, st, proj, 1, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil || rep.Merged != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	found := false
	for _, w := range rep.Warnings {
		if w == "Index.md is hand-written; left alone" {
			found = true
		}
	}
	if !found || !strings.Contains(readFile(t, st, "wiki/proj/Index.md"), "Hand-written overview.") {
		t.Fatalf("warnings %v", rep.Warnings)
	}
}

// Item 14: pending and submit agree on note ids.
func TestNoteIdsAreAnyStemWithoutSeparatorsOrControlCharacters(t *testing.T) {
	st, _ := newStore(t)
	body := "---\nkind: learning\n---\n\nThe worker restarts after five failed health checks and pages the owner.\n"
	putFile(t, st, "notes/proj/my note (2).md", body)
	putFile(t, st, "notes/proj/bad\x07name.md", body+"x")
	putFile(t, st, "notes/proj/.hidden.md", body+"y")
	if !usableID("my note (2)") || !usableID("é") || usableID("a/b") || usableID("a\\b") || usableID("a\x00b") || usableID("") || usableID(".x") {
		t.Fatal("usableID")
	}
	bs, err := Pending(context.Background(), st, proj, 0)
	if err != nil || len(bs) != 1 || len(bs[0].Notes) != 1 || bs[0].Notes[0].ID != "my note (2)" {
		t.Fatalf("%v %+v", err, bs)
	}
	if n := SkippedNotes(st.Repo, ""); n != 1 {
		t.Fatalf("skipped %d", n)
	}
	ws, _ := WaitingNotes(st.Repo)
	if len(ws) != 1 || ws[0].Notes != 1 {
		t.Fatalf("%+v", ws)
	}
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Body: "# W\n\n- " + bs[0].Notes[0].Text + "\n", Notes: []string{bs[0].Notes[0].ID}}}})
	if err != nil || rep.Merged != 1 || len(rep.Unknown) != 0 || noteCount(t, st, proj) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
}
