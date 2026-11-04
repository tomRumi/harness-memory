package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Committer is the only thing that runs git commit in the memory repository. Writers call Notify with the files they
// changed; one goroutine waits for a quiet moment, stages exactly those files and commits them in one commit. It never
// commits while a merge or a rebase is in progress, never when something other than a note or a fact is staged, and
// after any failure it tries again later (the files stay on disk, nothing is lost).
type Committer struct {
	repo    string
	quiet   time.Duration
	mu      sync.Mutex
	paths   map[string]bool
	timer   *time.Timer
	backoff time.Duration
	commits int
	lastErr error
	off     bool // not a git repository
}

func NewCommitter(repo string, quiet time.Duration) *Committer {
	_, err := os.Stat(filepath.Join(repo, ".git"))
	return &Committer{repo: repo, quiet: quiet, paths: map[string]bool{}, off: err != nil}
}

// Status reports the state for the health check.
func (c *Committer) Status() (commits int, lastErr error, busy, enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commits, c.lastErr, c.busyLocked(), !c.off
}

// Commits is the number of commits made so far.
func (c *Committer) Commits() int { c.mu.Lock(); defer c.mu.Unlock(); return c.commits }

// LastErr is the error of the last attempt, nil after a success.
func (c *Committer) LastErr() error { c.mu.Lock(); defer c.mu.Unlock(); return c.lastErr }

// Notify records the files (paths relative to the repository) that changed and schedules a commit.
func (c *Committer) Notify(rels ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range rels {
		c.paths[filepath.ToSlash(r)] = true
	}
	if c.off {
		return
	}
	c.armLocked(c.quiet)
}

func (c *Committer) armLocked(d time.Duration) {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(d, func() { c.Flush() })
}

// Busy reports whether git is in the middle of a merge or a rebase.
func (c *Committer) Busy() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.busyLocked() }

func (c *Committer) busyLocked() bool {
	g := filepath.Join(c.repo, ".git")
	for _, f := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(g, f)); err == nil {
			return true
		}
	}
	return false
}

func (c *Committer) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", c.repo}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// retryLocked schedules another attempt after a failure, with a delay that doubles up to five minutes.
func (c *Committer) retryLocked(err error) {
	c.lastErr = err
	if c.backoff == 0 {
		c.backoff = c.quiet
	} else if c.backoff < 5*time.Minute {
		c.backoff *= 2
	}
	c.armLocked(c.backoff)
}

// Flush commits now. It is safe to call from several goroutines: calls are serialised.
func (c *Committer) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off || len(c.paths) == 0 {
		return
	}
	if c.busyLocked() {
		c.armLocked(c.quiet) // wait the merge or rebase out; the files stay
		return
	}
	var add []string
	for p := range c.paths {
		if _, err := os.Stat(filepath.Join(c.repo, filepath.FromSlash(p))); err == nil {
			add = append(add, p)
		}
	}
	sort.Strings(add)
	if len(add) > 0 {
		if out, err := c.git(append([]string{"add", "--"}, add...)...); err != nil {
			c.retryLocked(fmt.Errorf("git add: %v: %s", err, out))
			return
		}
	}
	staged, err := c.git("diff", "--cached", "--name-only")
	if err != nil {
		c.retryLocked(fmt.Errorf("git diff --cached: %v: %s", err, staged))
		return
	}
	if staged == "" {
		c.paths, c.backoff, c.lastErr = map[string]bool{}, 0, nil
		return
	}
	for _, f := range strings.Split(staged, "\n") {
		if !strings.HasPrefix(f, "notes/") && !strings.HasPrefix(f, "facts/") {
			// the owner has staged something else; committing now would sweep it into a memory commit
			c.retryLocked(fmt.Errorf("not committing: %q is staged and is not a note or a fact", f))
			return
		}
	}
	msg := fmt.Sprintf("memory: %d change(s)", len(c.paths))
	if out, err := c.git("-c", "user.name=hms", "-c", "user.email=hms@localhost", "commit", "-q", "-m", msg); err != nil {
		c.retryLocked(fmt.Errorf("git commit: %v: %s", err, out))
		return
	}
	c.paths, c.backoff, c.lastErr = map[string]bool{}, 0, nil
	c.commits++
}

// Sweep notes the notes and facts that are on disk but not committed (for example after a crash) so that they are
// committed too. Temporary files of an interrupted write are skipped.
func (c *Committer) Sweep() {
	if c.off {
		return
	}
	out, err := exec.Command("git", "-C", c.repo, "ls-files", "--others", "--modified", "--exclude-standard", "--", "notes", "facts").Output()
	if err != nil {
		return
	}
	var rels []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f != "" && strings.HasSuffix(f, ".md") && !strings.HasPrefix(filepath.Base(f), ".") {
			rels = append(rels, f)
		}
	}
	if len(rels) > 0 {
		c.Notify(rels...)
	}
}

// CleanTemp removes temporary files of an interrupted write that are older than an hour.
func CleanTemp(repo string) {
	for _, d := range []string{"notes", "facts"} {
		filepath.WalkDir(filepath.Join(repo, d), func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasPrefix(e.Name(), ".tmp-") {
				if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > time.Hour {
					os.Remove(p)
				}
			}
			return nil
		})
	}
}
