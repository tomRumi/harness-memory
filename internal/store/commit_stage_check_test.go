package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The defence: git add that silently takes nothing is caught, reported, and nothing stays staged. A fake git that
// skips `add` stands for any reason (a case-folded folder, an ignore rule that appeared after the guard).
func TestCommitterCatchesAFileGitDidNotStage(t *testing.T) {
	st, _, repo := newStore(t)
	bin := t.TempDir()
	real, err := lookGit()
	if err != nil {
		t.Skip("no git")
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = add ] && exit 0; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	id, _ := st.Learn(context.Background(), "A note that is committed first, then deleted.", "p", "", "")
	st.Commit.Flush()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// a new file git will not stage, and a tracked file deleted whose deletion git will not stage
	os.Remove(filepath.Join(repo, "notes", "p", id+".md"))
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Pump.md"), []byte("# Pump\n\nA note that is committed first, then deleted.\n"), 0o644)
	c := NewCommitter(repo, time.Hour)
	c.Notify("notes/p/"+id+".md", "wiki/p/Pump.md")
	c.Flush()
	err = c.LastErr()
	if err == nil || !strings.Contains(err.Error(), "git did not stage ") || !strings.Contains(err.Error(), "run hms doctor") {
		t.Fatalf("the missing staging is not reported: %v", err)
	}
	if s := staged(t, repo); s != "" {
		t.Errorf("left staged: %q", s)
	}
	if c.Commits() != 0 {
		t.Error("committed anyway")
	}
}

func lookGit() (string, error) { return exec.LookPath("git") }
