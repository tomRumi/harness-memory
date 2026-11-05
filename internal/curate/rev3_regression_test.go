package curate

// Third review: a new page is never written over a file that exists (whatever the title comparison says), a page that
// git ignores is refused at write time, and a failed commit is reported and leaves nothing staged.

import (
	"context"
	"os"
	"path/filepath"
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

func exclude(t *testing.T, repo, rule string) {
	t.Helper()
	f := filepath.Join(repo, ".git", "info", "exclude")
	os.MkdirAll(filepath.Dir(f), 0o755)
	if err := os.WriteFile(f, []byte(rule+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An ignored page cannot be added; before, git add staged the other paths and failed, and the next commit carried the
// note deletion without the page.
func TestRev3_IgnoredPageGoesToUnsorted(t *testing.T) {
	st, repo := newStore(t)
	exclude(t, repo, "*Secret*")
	ns := learnN(t, st, proj, 1, tmplWorker)
	commits(t, st, repo)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Secret rotation", Summary: "s", Body: "# S\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil {
		t.Fatal(err)
	}
	p := rep.Pages[0]
	if p.Status != Unsorted || !strings.Contains(strings.Join(p.Problems, " "), "the page path is ignored by git") {
		t.Errorf("want Unsorted because the path is ignored, got %+v", p)
	}
	commits(t, st, repo)
	if s := git(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(s) != "" {
		t.Errorf("something is left staged: %q", s)
	}
	if st.Commit.LastErr() != nil {
		t.Errorf("commit error: %v", st.Commit.LastErr())
	}
	if !textSomewhere(t, repo, proj, ns[0]) || strings.TrimSpace(gitStatus(repo)) != "" {
		t.Errorf("note text lost or tree not clean: %q", gitStatus(repo))
	}
}

func TestRev3_IgnoredUnsortedFailsBeforeDeletingAnything(t *testing.T) {
	st, repo := newStore(t)
	exclude(t, repo, "Unsorted.md\n*Secret*")
	ns := learnN(t, st, proj, 1, tmplWorker)
	commits(t, st, repo)
	_, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Secret rotation", Summary: "s", Body: "# S\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err == nil || !strings.Contains(err.Error(), "ignored by git") {
		t.Fatalf("want an error naming the ignore rule, got %v", err)
	}
	if _, e := os.Stat(filepath.Join(repo, "notes", proj, ns[0].ID+".md")); e != nil {
		t.Errorf("the note was deleted: %v", e)
	}
	if s := gitStatus(repo); strings.TrimSpace(s) != "" {
		t.Errorf("tree changed: %q", s)
	}
}

// A failing commit (a hook that refuses) is named in the report, and nothing stays staged.
func TestRev3_FailedCommitIsReportedAndLeavesNothingStaged(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, proj, 1, tmplWorker)
	commits(t, st, repo)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte("#!/bin/sh\necho refused by the test hook\nexit 1\n"), 0o755)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: "# W\n\n" + bullets(ns), Notes: ids(ns)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rep.Warnings, "\n"), "committed: NO") || !strings.Contains(rep.String(), "committed: NO") {
		t.Errorf("the report does not say the commit failed: %v", rep.Warnings)
	}
	if s := git(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(s) != "" {
		t.Errorf("left staged: %q", s)
	}
}
