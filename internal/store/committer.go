package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Committer is the only thing that runs git commit in the memory repository. Writers call Notify after a change;
// one goroutine waits for a quiet moment, then commits everything under notes/ and facts/ in one commit. It never
// commits while a merge or a rebase is in progress, so it cannot disturb a pull.
type Committer struct {
	repo     string
	quiet    time.Duration
	mu       sync.Mutex
	pending  int
	timer    *time.Timer
	Commits  int // number of commits made (read it after Flush)
	LastErr  error
	disabled bool
}

func NewCommitter(repo string, quiet time.Duration) *Committer {
	_, err := os.Stat(filepath.Join(repo, ".git"))
	return &Committer{repo: repo, quiet: quiet, disabled: err != nil}
}

// Notify records that n files changed and schedules a commit after the quiet period.
func (c *Committer) Notify(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending += n
	if c.disabled {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(c.quiet, func() { c.Flush() })
}

// Busy reports whether git is in the middle of a merge or a rebase.
func (c *Committer) Busy() bool {
	g := filepath.Join(c.repo, ".git")
	for _, f := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(g, f)); err == nil {
			return true
		}
	}
	return false
}

// Flush commits now. It is safe to call from several goroutines: calls are serialised.
func (c *Committer) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled || c.pending == 0 {
		return
	}
	if c.Busy() {
		c.timer = time.AfterFunc(c.quiet, func() { c.Flush() }) // try again later; nothing is lost, the files stay
		return
	}
	n := c.pending
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", c.repo}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := git("add", "--", "notes", "facts"); err != nil {
		c.LastErr = fmt.Errorf("git add: %v: %s", err, out)
		return
	}
	staged, err := git("diff", "--cached", "--name-only")
	if err != nil {
		c.LastErr = fmt.Errorf("git diff --cached: %v: %s", err, staged)
		return
	}
	if staged == "" {
		c.pending = 0
		return
	}
	for _, f := range strings.Split(staged, "\n") {
		if !strings.HasPrefix(f, "notes/") && !strings.HasPrefix(f, "facts/") {
			// the owner has staged something else; committing now would sweep it into a memory commit
			c.LastErr = fmt.Errorf("not committing: %q is staged and is not a note or a fact", f)
			return
		}
	}
	msg := fmt.Sprintf("memory: %d change(s)", n)
	if out, err := git("-c", "user.name=hms", "-c", "user.email=hms@localhost", "commit", "-q", "-m", msg); err != nil {
		c.LastErr = fmt.Errorf("git commit: %v: %s", err, out)
		return
	}
	c.pending = 0
	c.Commits++
	c.LastErr = nil
}
