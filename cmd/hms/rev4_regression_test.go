package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fourth review, e12 through the real CLI: a hand-made, committed wiki/Proj folder (capital letters) makes the merge
// and `wiki index` refuse with the rename instruction, exit status 1, nothing changed.
func TestRev4_CapitalFolderRefusedByTheCLI(t *testing.T) {
	home, repo, ids := curateFixture(t)
	os.MkdirAll(filepath.Join(repo, "wiki", "Proj"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "Proj", "Old.md"), []byte("# Old\n\nWritten by hand.\n"), 0o644)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "hand page")
	file := filepath.Join(t.TempDir(), "answer.json")
	os.WriteFile(file, []byte(submission(ids)), 0o644)
	want := "the folder wiki/Proj has capital letters; project names are lower-case. Rename it to wiki/proj (git mv) and run hms rescan"
	for _, args := range [][]string{{"curate", "-submit", file}, {"wiki", "index", "-p", "proj"}} {
		_, errOut, c := hms(t, home, deadURL(), noModel, "", args...)
		if c == 0 || !strings.Contains(errOut, want) {
			t.Errorf("%v: exit %d, stderr %q", args, c, errOut)
		}
	}
	if s := strings.TrimSpace(gitIn(t, repo, "status", "--porcelain", "-uall")); s != "" {
		t.Errorf("something changed: %q", s)
	}
	for _, id := range ids[:3] {
		if _, err := os.Stat(filepath.Join(repo, "notes", "proj", id+".md")); err != nil {
			t.Errorf("note %s: %v", id, err)
		}
	}
}
