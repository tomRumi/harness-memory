package curate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fourth review, repro e12: notes exist, then a folder wiki/MyProj appears (Obsidian), then the merge runs. It used to
// say "merged" with exit 0 while the page was in no commit (git, case-insensitive, did not stage the lower-case path).
// Now the whole merge is refused before anything changes; the notes stay where they are, the repository stays clean.
func TestRev4_E12_MergeIntoCapitalFolderIsRefusedAndChangesNothing(t *testing.T) {
	st, repo := newStore(t)
	ns := learnN(t, st, "myproj", 2, tmplWorker)
	commits(t, st, repo)
	// a hand-made, committed page makes git remember the folder as "MyProj"
	if err := os.MkdirAll(filepath.Join(repo, "wiki", "MyProj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "wiki", "MyProj", "Old.md"), []byte("# Old\n\nA page written by hand.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "hand page")
	rep, err := Submit(context.Background(), st, Submission{Project: "myproj", Pages: []Page{{Title: "Pump", Summary: "s", Body: "# Pump\n\n" + bullets(ns), Notes: ids(ns)}}})
	want := "the folder wiki/MyProj has capital letters; project names are lower-case. Rename it to wiki/myproj (git mv) and run hms rescan"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got err=%v rep=%+v", want, err, rep)
	}
	for _, n := range ns {
		if _, e := os.Stat(filepath.Join(repo, "notes", "myproj", n.ID+".md")); e != nil {
			t.Errorf("note %s was removed: %v", short(n.ID), e)
		}
	}
	if s := gitStatus(repo); strings.TrimSpace(s) != "" {
		t.Errorf("the repository is not clean: %q", s)
	}
	// the owner renames the folder as told; the next merge works and is committed
	git(t, repo, "mv", "wiki/MyProj/Old.md", "wiki/myproj-tmp.md")
	os.Remove(filepath.Join(repo, "wiki", "MyProj"))
	os.MkdirAll(filepath.Join(repo, "wiki", "myproj"), 0o755)
	git(t, repo, "mv", "wiki/myproj-tmp.md", "wiki/myproj/Old.md")
	git(t, repo, "commit", "-q", "-m", "renamed")
	if _, err := Submit(context.Background(), st, Submission{Project: "myproj", Pages: []Page{{Title: "Pump", Summary: "s", Body: "# Pump\n\n" + bullets(ns), Notes: ids(ns)}}}); err != nil {
		t.Fatal(err)
	}
	commits(t, st, repo)
	if s := gitStatus(repo); strings.TrimSpace(s) != "" || st.Commit.LastErr() != nil {
		t.Errorf("after the fix: status %q, last error %v", s, st.Commit.LastErr())
	}
}
