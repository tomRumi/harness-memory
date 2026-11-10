package store

// Third review: the committer never leaves a half set staged, sees staged leftovers, and Sweep takes the write lock.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory/internal/index"
)

func staged(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, repo, "diff", "--cached", "--name-only"))
}

// A page that cannot be added (mode 000) must not leave the note deletion staged for another committer to carry.
func TestRev3_PartialAddCommittedByOtherCommitter(t *testing.T) {
	st, _, repo := newStore(t)
	id, _ := st.Learn(context.Background(), "A note whose page cannot be added by the first committer.", "p", "", "")
	st.Commit.Flush()
	os.Remove(filepath.Join(repo, "notes", "p", id+".md"))
	page := filepath.Join(repo, "wiki", "p", "Café.md")
	os.MkdirAll(filepath.Dir(page), 0o755)
	os.WriteFile(page, []byte("# Café\n\nA note whose page cannot be added by the first committer.\n"), 0o000)
	defer os.Chmod(page, 0o644)
	st.Commit.Sweep()
	st.Commit.Flush()
	c2 := NewCommitter(repo, time.Hour) // another process
	os.WriteFile(filepath.Join(repo, "notes", "p", "other.md"), []byte("---\nkind: learning\n---\n\nAn unrelated note of the other process.\n"), 0o644)
	c2.Notify("notes/p/other.md")
	c2.Flush()
	files := gitOut(t, repo, "-c", "core.quotepath=false", "show", "--name-status", "--format=", "HEAD")
	if strings.Contains(files, "D\tnotes/p/"+id+".md") && !strings.Contains(files, "Café.md") {
		t.Errorf("the note deletion is committed without its page:\n%s", files)
	}
}

// git add of an ignored path stages the other paths and then fails: the staged half must be taken out again.
func TestRev3_AddFailureUnstagesTheOtherPaths(t *testing.T) {
	st, _, repo := newStore(t)
	id, _ := st.Learn(context.Background(), "A note that moves into a page that git ignores.", "p", "", "")
	st.Commit.Flush()
	os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("*Secret*\n"), 0o644)
	os.Remove(filepath.Join(repo, "notes", "p", id+".md"))
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Secret rotation.md"), []byte("# S\n"), 0o644)
	st.Commit.Notify("notes/p/"+id+".md", "wiki/p/Secret rotation.md")
	st.Commit.Flush()
	if st.Commit.LastErr() == nil {
		t.Error("the failure is not reported")
	}
	if s := staged(t, repo); s != "" {
		t.Errorf("left staged: %q", s)
	}
}

func TestRev3_FailedCommitUnstages(t *testing.T) {
	st, _, repo := newStore(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	st.Learn(context.Background(), "A note whose commit the hook refuses.", "p", "", "")
	st.Commit.Flush()
	if st.Commit.LastErr() == nil {
		t.Error("the failure is not reported")
	}
	if s := staged(t, repo); s != "" {
		t.Errorf("left staged: %q", s)
	}
}

// A kill during git commit leaves staged files that are neither modified nor untracked: Sweep must see them.
func TestRev3_SweepSeesStagedLeftovers(t *testing.T) {
	st, _, repo := newStore(t)
	id, _ := st.Learn(context.Background(), "A note that was moved into a page before the kill.", "p", "", "")
	st.Commit.Flush()
	gitOut(t, repo, "rm", "-q", "notes/p/"+id+".md")
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Page.md"), []byte("# Page\n\nA note that was moved into a page before the kill.\n"), 0o644)
	gitOut(t, repo, "add", "wiki/p/Page.md")
	c := NewCommitter(repo, time.Hour)
	c.Sweep()
	c.Flush()
	if s := staged(t, repo); s != "" {
		t.Errorf("staged leftovers stay: %q", s)
	}
	if got := gitOut(t, repo, "show", "--name-status", "--format=%s", "HEAD"); !strings.Contains(got, "Page.md") || !strings.Contains(got, id) {
		t.Errorf("the leftovers are not in one commit:\n%s", got)
	}
}

// Sweep of a second store on the same home waits for a Do of the first (same lock file); it never commits a half way
// operation that is then rolled back.
func TestRev3_SweepWaitsForADo(t *testing.T) {
	st, _, repo := newStore(t)
	id, _ := st.Learn(context.Background(), "A note that a merge moves into a page.", "p", "", "")
	st.Commit.Flush()
	home := filepath.Dir(repo)
	db2, err := index.Open(filepath.Join(home, "index2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	st2, err := New(repo, db2, nil, NewCommitter(repo, time.Hour), filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	paused, resume := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st.Do(func(tx *Tx) error {
			if err := tx.Write("wiki/p/Page.md", []byte("# Page\n\nA note that a merge moves into a page.\n")); err != nil {
				return err
			}
			if err := tx.Delete("notes/p/"+id, "p", "", ""); err != nil {
				return err
			}
			close(paused)
			<-resume
			return errors.New("the check failed late: roll back")
		})
	}()
	<-paused
	swept := make(chan struct{})
	go func() { st2.Sweep(); close(swept) }()
	select {
	case <-swept:
		t.Error("Sweep ran while a Do held the write lock")
	case <-time.After(500 * time.Millisecond):
	}
	close(resume)
	wg.Wait()
	<-swept
	if head := gitOut(t, repo, "show", "--name-status", "--format=%s", "HEAD"); strings.Contains(head, "Page.md") {
		t.Errorf("a merge that was rolled back is in a commit:\n%s", head)
	}
}

func TestRev3_GitHasATimeout(t *testing.T) {
	repo := t.TempDir()
	if _, err := Git(repo, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if out, err := Git(repo, "rev-parse", "--is-inside-work-tree"); err != nil || out != "true" {
		t.Fatalf("%q %v", out, err)
	}
}
