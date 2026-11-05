package store

// Fourth review: the service writes the lower-case path, git may see another spelling. The write is refused before
// anything changes, and the committer checks that git really took every file.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capFolder makes tree/MyProj by hand (what an Obsidian user does) and says whether the file system folds case.
func capFolder(t *testing.T, repo, tree string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, tree, "MyProj"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func wantFolderRefusal(t *testing.T, err error, tree string) {
	t.Helper()
	want := "the folder " + tree + "/MyProj has capital letters; project names are lower-case. Rename it to " + tree + "/myproj (git mv) and run hms rescan"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
}

func looseFiles(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, repo, "status", "--porcelain", "-uall"))
}

// e12 of the review: learn, AddFact, Retire and the index page refuse, and nothing is created, changed or committed.
func TestRev4_CapitalFolderRefusesEveryWrite(t *testing.T) {
	ctx := context.Background()
	st, _, repo := newStore(t)
	id, err := st.Learn(ctx, "A note in the lower-case folder that is kept.", "other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	capFolder(t, repo, "notes")
	capFolder(t, repo, "facts")
	capFolder(t, repo, "wiki")
	commits := st.Commit.Commits()

	_, err = st.Learn(ctx, "A note for the project whose folder has capitals.", "MyProj", "", "")
	wantFolderRefusal(t, err, "notes")
	_, err = st.AddFact(ctx, "A fact for the project whose folder has capitals.", "MyProj", "", 0.5)
	wantFolderRefusal(t, err, "facts")
	_, err = st.WriteIndexPage(ctx, "myproj")
	wantFolderRefusal(t, err, "wiki")
	// a superseding learn into the good project may not touch anything either when its target is in the bad one
	if _, err = st.Learn(ctx, "Replaces the note, in the capital folder.", "MyProj", "", id); err == nil {
		t.Error("supersede through the capital folder was not refused")
	}
	if entries, _ := os.ReadDir(filepath.Join(repo, "notes", "MyProj")); len(entries) != 0 {
		t.Errorf("files were written into the capital folder: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(repo, "notes", "other", id+".md")); err != nil {
		t.Errorf("the other project's note was changed: %v", err)
	}
	st.Commit.Flush()
	if st.Commit.Commits() != commits {
		t.Error("a refused write was committed")
	}
}

// Retire of a note that lies in a capital-letter folder is refused too (its delete would not be staged either).
func TestRev4_RetireInCapitalFolderRefused(t *testing.T) {
	ctx := context.Background()
	st, _, repo := newStore(t)
	capFolder(t, repo, "notes")
	rel := "notes/MyProj/abcdef12-0000.md"
	body := "---\nkind: learning\ncreated: 2020-01-01T10:00:00Z\nsource: learn\n---\n\nA hand-placed note in a capital folder.\n"
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.afterWrite(ctx, rel); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	out, err := st.Retire(ctx, "abcdef12", "", "")
	if err == nil {
		t.Fatalf("retire was not refused: %q", out)
	}
	wantFolderRefusal(t, err, "notes")
	if _, serr := os.Stat(filepath.Join(repo, filepath.FromSlash(rel))); serr != nil {
		t.Errorf("the note was removed: %v", serr)
	}
}
