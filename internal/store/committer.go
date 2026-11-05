package store

import (
	"context"
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
	mu      sync.Mutex       // guards the fields below; never held while git runs
	runMu   sync.Mutex       // one flush at a time
	paths   map[string]int64 // path -> number of the last Notify that named it
	msgs    []string         // commit messages given by the Notify calls since the last commit, in order; none: the default
	seq     int64
	timer   *time.Timer
	backoff time.Duration
	commits int
	lastErr error
	off     bool // not a git repository
	stopped bool
}

func NewCommitter(repo string, quiet time.Duration) *Committer {
	_, err := os.Stat(filepath.Join(repo, ".git"))
	return &Committer{repo: repo, quiet: quiet, paths: map[string]int64{}, off: err != nil}
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
func (c *Committer) Notify(rels ...string) { c.NotifyMsg("", rels...) }

// NotifyMsg is Notify with a commit message. When several Notify calls fall into one commit, the messages of all of them
// call that carried one is kept: the first is the subject of the commit, the others follow in the body (the whole
// message is cut at MaxMessage characters). With none the message is "memory: N change(s)".
func (c *Committer) NotifyMsg(msg string, rels ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg != "" {
		c.msgs = append(c.msgs, msg)
	}
	for _, r := range rels {
		c.seq++
		c.paths[filepath.ToSlash(r)] = c.seq
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
	if c.stopped {
		return
	}
	c.timer = time.AfterFunc(d, func() { c.Flush() })
}

// Detached reports whether HEAD points at no branch (a checked-out commit or tag). Commits then would be lost the
// moment the owner switches branch, so none are made. An unborn branch (no commit yet) is a branch.
func (c *Committer) Detached() bool {
	if c.off {
		return false
	}
	_, err := c.git("symbolic-ref", "-q", "HEAD")
	return err != nil
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

// GitTimeout bounds every git call. A hung hook or a signing prompt must not hold up the service.
const GitTimeout = 30 * time.Second

func (c *Committer) git(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("git %s: timed out after %s", args[0], GitTimeout)
	}
	return strings.TrimSpace(string(out)), err
}

// retryLocked schedules another attempt after a failure, with a delay that doubles up to five minutes.
func (c *Committer) retryLocked(err error) {
	c.lastErr = err
	if c.backoff == 0 {
		c.backoff = c.quiet
	} else {
		c.backoff *= 2
	}
	if c.backoff > 5*time.Minute {
		c.backoff = 5 * time.Minute
	}
	c.armLocked(c.backoff)
}

// Flush commits now. It is safe to call from several goroutines: calls are serialised. The state lock is not held
// while git runs, so writers calling Notify and the health check never wait for git.
func (c *Committer) Flush() {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	c.mu.Lock()
	if c.off || len(c.paths) == 0 {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	detached := c.Detached() // runs git: the state lock is not held
	c.mu.Lock()
	if detached {
		c.armLocked(c.quiet) // the owner may check out a branch again; the files stay
		c.mu.Unlock()
		return
	}
	if c.busyLocked() {
		c.armLocked(c.quiet) // wait the merge or rebase out; the files stay
		c.mu.Unlock()
		return
	}
	snap := make([]string, 0, len(c.paths))
	seen := make(map[string]int64, len(c.paths))
	for p, n := range c.paths {
		snap = append(snap, p)
		seen[p] = n
	}
	msgs := append([]string(nil), c.msgs...)
	c.mu.Unlock()
	sort.Strings(snap)

	err := c.commit(snap, joinMessages(msgs))

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.retryLocked(err)
		return
	}
	for _, p := range snap {
		if c.paths[p] == seen[p] {
			delete(c.paths, p) // a path changed again while git ran keeps its newer number and goes in the next round
		}
	}
	c.backoff, c.lastErr = 0, nil
	c.msgs = c.msgs[len(msgs):]
	if len(c.paths) > 0 {
		c.armLocked(c.quiet)
	}
}

// MaxMessage is the longest commit message, in characters.
const MaxMessage = 2000

// joinMessages makes one commit message of the messages of several Notify calls: the first is the subject, the others
// follow in the body, and the whole is cut at MaxMessage characters. No message gives "" (the default is used).
func joinMessages(msgs []string) string {
	out := strings.Join(msgs, "\n\n")
	if r := []rune(out); len(r) > MaxMessage {
		out = string(r[:MaxMessage-1]) + "…"
	}
	return out
}

// commit stages the given paths and commits them. A tracked path that no longer exists is staged as a deletion. Only
// notes, facts and wiki entries are ever named. It returns nil when there was nothing to commit.
func (c *Committer) commit(snap []string, msg string) error {
	var add, gone []string
	for _, p := range snap {
		if !strings.HasPrefix(p, "notes/") && !strings.HasPrefix(p, "facts/") && !strings.HasPrefix(p, "wiki/") {
			continue
		}
		if _, err := os.Stat(filepath.Join(c.repo, filepath.FromSlash(p))); err == nil {
			add = append(add, p)
		} else {
			gone = append(gone, p)
		}
	}
	if len(gone) > 0 {
		// a file that was created and deleted before it was ever committed is unknown to git and needs no staging
		out, err := c.git(append([]string{"--literal-pathspecs", "ls-files", "-z", "--"}, gone...)...)
		if err != nil {
			return fmt.Errorf("git ls-files: %v: %s", err, out)
		}
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				add = append(add, f)
			}
		}
	}
	if len(add) > 0 {
		if out, err := c.git(append([]string{"--literal-pathspecs", "add", "-A", "--"}, add...)...); err != nil {
			return fmt.Errorf("git add: %v: %s", err, out)
		}
	}
	staged, err := c.git("diff", "--cached", "--name-only", "-z") // -z: a path with accents is not shown quoted
	if err != nil {
		return fmt.Errorf("git diff --cached: %v: %s", err, staged)
	}
	if staged == "" {
		return nil
	}
	for _, f := range strings.Split(staged, "\x00") {
		if f != "" && !strings.HasPrefix(f, "notes/") && !strings.HasPrefix(f, "facts/") && !strings.HasPrefix(f, "wiki/") {
			// the owner has staged something else; committing now would sweep it into a memory commit
			return fmt.Errorf("not committing: %q is staged and is not a note, a fact or a wiki entry", f)
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("memory: %d change(s)", len(snap))
	}
	if out, err := c.git("-c", "user.name=hms", "-c", "user.email=hms@localhost", "commit", "-q", "-m", msg); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, out)
	}
	c.mu.Lock()
	c.commits++
	c.mu.Unlock()
	return nil
}

// Sweep notes the notes and facts that are on disk but not committed (for example after a crash) so that they are
// committed too. Temporary files of an interrupted write are skipped.
func (c *Committer) Sweep() {
	if c.off {
		return
	}
	out, err := exec.Command("git", "-C", c.repo, "ls-files", "--others", "--modified", "--exclude-standard", "--", "notes", "facts", "wiki").Output()
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
	for _, d := range []string{"notes", "facts", "wiki"} {
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

// Stop makes the last commit now and ends the timers; nothing runs after it returns.
func (c *Committer) Stop() {
	c.mu.Lock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
	c.Flush()
}
