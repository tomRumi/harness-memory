package install

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A first commit that fails (a hook that refuses) is reported by init, and the .gitignore is not left staged: a staged
// .gitignore would make the committer refuse every later memory commit.
func TestInitFailedFirstCommitLeavesNothingStaged(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	tpl := t.TempDir()
	os.MkdirAll(filepath.Join(tpl, "hooks"), 0o755)
	os.WriteFile(filepath.Join(tpl, "hooks", "pre-commit"), []byte("#!/bin/sh\necho refused by the test hook\nexit 1\n"), 0o755)
	t.Setenv("GIT_TEMPLATE_DIR", tpl)
	p := paths(t)
	var b bytes.Buffer
	err := Init(p, InitOptions{Out: &b})
	if err == nil || !strings.Contains(err.Error(), "committed: NO") {
		t.Fatalf("init must report the failed commit, got %v\n%s", err, b.String())
	}
	if out, _ := exec.Command("git", "-C", p.Repo, "diff", "--cached", "--name-only").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("left staged: %q", out)
	}
	if _, e := os.Stat(p.DB); e != nil {
		t.Error("the rest of init did not run: no index")
	}
}

func TestDoctorWarnsOnAnIndexLock(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	os.WriteFile(lock, nil, 0o644)
	old := time.Now().Add(-3 * time.Minute)
	os.Chtimes(lock, old, old)
	var lines []string
	doctorRepoState(repo, func(level, what, detail string) { lines = append(lines, level+" "+what+" "+detail) })
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN commits .git/index.lock exists (3m") || !strings.Contains(lines[0], "blocked") {
		t.Fatalf("%v", lines)
	}
}
