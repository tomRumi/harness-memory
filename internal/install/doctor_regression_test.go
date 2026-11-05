package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Item 15: doctor warns about a detached HEAD; init no longer makes raw/.
func TestDoctorRepoStateWarnsOnADetachedHead(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	var lines []string
	line := func(level, what, detail string) { lines = append(lines, level+" "+what+" "+detail) }
	doctorRepoState(repo, line) // unborn branch: fine
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "OK ") {
		t.Fatalf("unborn: %v", lines)
	}
	run("commit", "-q", "--allow-empty", "-m", "x")
	run("checkout", "-q", "--detach")
	lines = nil
	doctorRepoState(repo, line)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN commits HEAD is detached") {
		t.Fatalf("detached: %v", lines)
	}
}

func TestInitDoesNotMakeAnUnusedRawDirectory(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	for _, d := range []string{"wiki", "notes", "facts"} {
		if fi, err := os.Stat(filepath.Join(p.Repo, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s missing", d)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Repo, "raw")); err == nil {
		t.Error("raw/ was created")
	}
}
