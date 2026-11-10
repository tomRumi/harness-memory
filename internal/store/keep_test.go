package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slowStore is a store whose committer waits an hour, so nothing is committed unless the store asks for it.
func slowStore(t *testing.T) (*Store, string) {
	st, _, repo := newStore(t)
	st.Commit = NewCommitter(repo, time.Hour)
	return st, repo
}

func blockCommits(t *testing.T, repo string) {
	t.Helper()
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused by hook\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRetireRightAfterLearnKeepsTheTextInGit(t *testing.T) {
	st, repo := slowStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "Saved and retired inside the quiet delay, yet kept.", "p", "", "")
	if out, err := st.Retire(ctx, id[:8], "", "p"); err != nil || !strings.HasPrefix(out, "deleted ") {
		t.Fatalf("%q %v", out, err)
	}
	st.Commit.Flush()
	if !strings.Contains(gitOut(t, repo, "log", "-p", "--all", "--", "notes/p/"+id+".md"), "inside the quiet delay") {
		t.Fatal("the text is in no commit")
	}
}

func TestSupersedeRightAfterLearnKeepsTheOldTextInGit(t *testing.T) {
	st, repo := slowStore(t)
	ctx := context.Background()
	old, _ := st.Learn(ctx, "The old statement that is replaced at once.", "p", "", "")
	res, err := st.Learn(ctx, "The new statement.", "p", "", old[:8])
	if err != nil || !strings.Contains(res, "replaced "+old[:8]) {
		t.Fatalf("%q %v", res, err)
	}
	st.Commit.Flush()
	if !strings.Contains(gitOut(t, repo, "log", "-p", "--all", "--", "notes/p/"+old+".md"), "replaced at once") {
		t.Fatal("the old text is in no commit")
	}
}

func TestRetireDeletesNothingWhenCommitsFail(t *testing.T) {
	st, repo := slowStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "A note that cannot be committed.", "p", "", "")
	blockCommits(t, repo)
	out, err := st.Retire(ctx, id[:8], "", "p")
	if err != nil || !strings.Contains(out, "not retired") || !strings.Contains(out, "committing failed") || !strings.Contains(out, "still there") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "notes", "p", id+".md")); err != nil {
		t.Fatalf("the file must still be there: %v", err)
	}
	if _, msg := st.resolve(id[:8], "p"); msg != "" {
		t.Fatalf("the note left the index: %s", msg)
	}
}

func TestSupersedeStoresTheNewNoteButReplacesNothingWhenCommitsFail(t *testing.T) {
	st, repo := slowStore(t)
	ctx := context.Background()
	old, _ := st.Learn(ctx, "An old note that cannot be committed.", "p", "", "")
	blockCommits(t, repo)
	res, err := st.Learn(ctx, "A new note that is stored anyway.", "p", "", old[:8])
	if err != nil || !strings.Contains(res, "nothing was replaced") || !strings.Contains(res, "committing failed") {
		t.Fatalf("%q %v", res, err)
	}
	newID := strings.Fields(res)[0]
	for _, id := range []string{old, newID} {
		if _, err := os.Stat(filepath.Join(repo, "notes", "p", id+".md")); err != nil {
			t.Fatalf("%s must be on disk: %v", id, err)
		}
	}
}

func TestRetireDeletesNothingInAMemoryWithoutGit(t *testing.T) {
	st, repo := slowStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "A note in a folder that is not a git repository.", "p", "", "")
	os.RemoveAll(filepath.Join(repo, ".git"))
	st.Commit = NewCommitter(repo, time.Hour)
	out, err := st.Retire(ctx, id[:8], "", "p")
	if err != nil || !strings.Contains(out, "not retired") || !strings.Contains(out, "not a git repository") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "notes", "p", id+".md")); err != nil {
		t.Fatalf("the file must still be there: %v", err)
	}
	st.Commit = nil
	if out, _ := st.Retire(ctx, id[:8], "", "p"); !strings.Contains(out, "not retired") {
		t.Fatalf("no committer: %q", out)
	}
}
