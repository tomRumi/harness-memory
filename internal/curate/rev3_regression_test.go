package curate

// Third review: a new page is never written over a file that exists (whatever the title comparison says).

import (
	"context"
	"strings"
	"testing"
)

// Cherokee letters: the file system (APFS) folds them, x/text cases.Fold swaps their case instead. The file system is
// the final truth for "exists".
func TestRev3_CherokeeNewTitleNeverOverwritesAFile(t *testing.T) {
	st, repo := newStore(t)
	old := "# ᏣᎳᎩ\n\nYears of hand-written text that must survive the merge.\n"
	putFile(t, st, "wiki/proj/ᏣᎳᎩ.md", old)
	ns := learnN(t, st, proj, 1, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "ꮳꮃꭹ", Summary: "s", Body: "# T\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(allWiki(t, repo), "Years of hand-written text") {
		t.Errorf("the existing page was overwritten by a new title: %+v", rep.Pages)
	}
	if rep.Pages[0].Status != Stale || len(rep.Left) != 1 {
		t.Errorf("want stale with the note left pending, got %+v left=%v", rep.Pages[0], rep.Left)
	}
}

func TestRev3_CherokeeTwoPagesOfOneSubmissionAreOneFile(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 2, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "ᏣᎳᎩ", Summary: "s", Body: "# S\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "ꮳꮃꭹ", Summary: "s", Body: "# S\n\n" + bullets(ns[1:]), Notes: ids(ns[1:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if !textSomewhere(t, repo, proj, n) {
			t.Errorf("note %s lost", short(n.ID))
		}
	}
	if rep.Pages[1].Status != Unsorted {
		t.Errorf("the second page of the same file must go to Unsorted, got %+v", rep.Pages[1])
	}
}
