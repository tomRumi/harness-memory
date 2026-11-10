package store

// Second review: adversarial tests of the new store code. A failing test with "DEFECT" is a finding.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// R01: a lock holder killed with SIGKILL releases the lock (flock dies with the process).
func TestCrashedLockHolder(t *testing.T) {
	if os.Getenv("HMS_TEST_HOLD_LOCK") != "" {
		unlock, err := lockFile(os.Getenv("HMS_TEST_HOLD_LOCK"))
		if err != nil {
			os.Exit(3)
		}
		_ = unlock
		fmt.Println("held")
		time.Sleep(time.Hour)
		return
	}
	st, _, repo := newStore(t)
	lock := filepath.Join(filepath.Dir(repo), "write.lock")
	cmd := exec.Command(os.Args[0], "-test.run", "^TestCrashedLockHolder$")
	cmd.Env = append(os.Environ(), "HMS_TEST_HOLD_LOCK="+lock)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	out.Read(buf)
	// while held, a Learn must wait
	done := make(chan error, 1)
	go func() {
		_, err := st.Learn(context.Background(), "A note written while another process holds the lock.", "p", "", "")
		done <- err
	}()
	select {
	case <-done:
		t.Errorf("DEFECT Learn did not wait for the lock held by another process")
	case <-time.After(500 * time.Millisecond):
	}
	cmd.Process.Kill()
	cmd.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DEFECT lock not released after the holder was killed")
	}
}

// R02: a lock file that cannot be opened. Report what a save does.
func TestLockFileUnopenable(t *testing.T) {
	st, _, repo := newStore(t)
	lock := filepath.Join(filepath.Dir(repo), "write.lock")
	os.WriteFile(lock, nil, 0o444)
	defer os.Chmod(lock, 0o644)
	_, err := st.Learn(context.Background(), "A note while the lock file is read-only.", "p", "", "")
	t.Logf("learn with a read-only write.lock: err=%v", err)
	if err == nil {
		t.Logf("saved without the lock")
	}
	// the in-process mutex must not stay held after the failure
	os.Chmod(lock, 0o644)
	ch := make(chan error, 1)
	go func() {
		_, err := st.Learn(context.Background(), "A second note after the lock file is fixed.", "p", "", "")
		ch <- err
	}()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DEFECT mutex left held after a lock-open failure")
	}
}

// R03: two stores in one process (two processes in real life) with mixed writes and Do: no deadlock.
func TestNoDeadlockTwoStores(t *testing.T) {
	st, _, repo := newStore(t)
	home := filepath.Dir(repo)
	st2, err := New(repo, st.DB, &bag{}, NewCommitter(repo, 20*time.Millisecond), filepath.Join(home, "events2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := st
			if i%2 == 1 {
				s = st2
			}
			switch i % 4 {
			case 0, 1:
				s.Learn(context.Background(), fmt.Sprintf("Concurrent note %d about the relay and the queue.", i), "p", "", "")
			case 2:
				s.Do(func(tx *Tx) error { return tx.Write(fmt.Sprintf("wiki/p/P%d.md", i), []byte("# P\n\ntext\n")) })
			case 3:
				s.WriteIndexPage(context.Background(), "p")
			}
		}(i)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("DEFECT deadlock\n%s", buf[:n])
	}
}

// R04: the write lock is not held while git runs (a slow pre-commit hook).
func TestLockNotHeldDuringSlowGit(t *testing.T) {
	st, _, repo := newStore(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	os.WriteFile(hook, []byte("#!/bin/sh\nsleep 3\n"), 0o755)
	st.Learn(context.Background(), "First note before the slow commit runs.", "p", "", "")
	go st.Commit.Flush()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if _, err := st.Learn(context.Background(), "Second note while git is slow.", "p", "", ""); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("DEFECT a save waited %v for a slow git", d)
	}
}

// R05: Do writes a file twice and deletes a note, then fails: the first version comes back, once.
func TestRollbackTwoWritesSameFile(t *testing.T) {
	st, _, repo := newStore(t)
	p := filepath.Join(repo, "wiki", "p", "A.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("# A\n\noriginal\n"), 0o644)
	st.DB.Rescan(repo)
	id, _ := st.Learn(context.Background(), "A note the failed merge deletes and must restore.", "p", "", "")
	err := st.Do(func(tx *Tx) error {
		tx.Write("wiki/p/A.md", []byte("# A\n\nv1\n"))
		tx.Write("wiki/p/A.md", []byte("# A\n\nv2\n"))
		tx.Delete("notes/p/"+id, "p", "merged", "x")
		tx.Write("wiki/p/A.md", []byte("# A\n\nv3\n"))
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("no error")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "# A\n\noriginal\n" {
		t.Errorf("DEFECT restored the wrong version: %q", b)
	}
	if _, err := os.Stat(filepath.Join(repo, "notes", "p", id+".md")); err != nil {
		t.Errorf("DEFECT note not restored")
	}
	var n int
	st.ev.QueryRow(`SELECT count(*) FROM events WHERE kind='merged' AND node_id=?`, "notes/p/"+id).Scan(&n)
	if n > 0 {
		t.Errorf("DEFECT the event 'merged' of a rolled-back Do stays in events.db (%d)", n)
	}
	// and a Do that succeeds records it
	id2, _ := st.Learn(context.Background(), "A second note that a successful Do deletes.", "p", "", "")
	if err := st.Do(func(tx *Tx) error { return tx.Delete("notes/p/"+id2, "p", "merged", "x") }); err != nil {
		t.Fatal(err)
	}
	st.ev.QueryRow(`SELECT count(*) FROM events WHERE kind='merged' AND node_id=?`, "notes/p/"+id2).Scan(&n)
	if n != 1 {
		t.Errorf("a successful Do records its event once, got %d", n)
	}
}

// R06: a file that existed but was unreadable when first touched is deleted by the rollback.
func TestRollbackUnreadableFile(t *testing.T) {
	st, _, repo := newStore(t)
	p := filepath.Join(repo, "wiki", "p", "Secret.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("# Secret\n\nhand text\n"), 0o000)
	st.Do(func(tx *Tx) error {
		tx.Write("wiki/p/Secret.md", []byte("x"))
		return errors.New("fail")
	})
	os.Chmod(p, 0o644)
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "hand text") {
		t.Errorf("DEFECT an unreadable page touched by a failed Do is gone or replaced: err=%v content=%q", err, b)
	}
}

// R07: a file changed by someone else between the tx write and the failure is overwritten by the rollback.
func TestRollbackOverwritesForeignEdit(t *testing.T) {
	st, _, repo := newStore(t)
	p := filepath.Join(repo, "wiki", "p", "A.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("orig\n"), 0o644)
	st.Do(func(tx *Tx) error {
		tx.Write("wiki/p/A.md", []byte("tx\n"))
		os.WriteFile(p, []byte("owner edit in Obsidian\n"), 0o644)
		return errors.New("fail")
	})
	b, _ := os.ReadFile(p)
	t.Logf("after rollback: %q (the edit made during the failing Do is replaced)", b)
}

// R08: Sweep after a crash must cover a wiki page with an accented name.
func TestSweepAccentedWikiPage(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Café.md"), []byte("# Café\n\ntext\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Plain.md"), []byte("# Plain\n\ntext\n"), 0o644)
	st.Commit.Sweep()
	st.Commit.Flush()
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("DEFECT Sweep left uncommitted: %q", out)
	}
}

// R09: detached HEAD: no commit; back on a branch the files are committed with all messages, once.
func TestDetachedThenBranch(t *testing.T) {
	st, _, repo := newStore(t)
	st.Learn(context.Background(), "Note one before detaching the head.", "p", "", "")
	st.Commit.Flush()
	exec.Command("git", "-C", repo, "checkout", "-q", "--detach").Run()
	st.Learn(context.Background(), "Note two on a detached head.", "p", "", "")
	st.Do(func(tx *Tx) error {
		tx.Write("wiki/p/X.md", []byte("# X\n\nx\n"))
		tx.Finish(context.Background(), "curate p: special message")
		return nil
	})
	st.Commit.Flush()
	n1, _ := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output()
	exec.Command("git", "-C", repo, "checkout", "-q", "main").Run()
	time.Sleep(200 * time.Millisecond)
	st.Commit.Flush()
	n2, _ := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output()
	log, _ := exec.Command("git", "-C", repo, "log", "-1", "--format=%B").Output()
	st2, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	t.Logf("commits detached=%s after=%s status=%q\nlast message:\n%s", strings.TrimSpace(string(n1)), strings.TrimSpace(string(n2)), st2, log)
	if !strings.Contains(string(log), "special message") || strings.TrimSpace(string(st2)) != "" {
		t.Errorf("DEFECT after returning to a branch: message or files missing")
	}
	st.Learn(context.Background(), "Note three after the branch is back.", "p", "", "")
	st.Commit.Flush()
	log2, _ := exec.Command("git", "-C", repo, "log", "-1", "--format=%B").Output()
	if strings.Contains(string(log2), "special message") {
		t.Errorf("DEFECT old message repeated in a later commit: %s", log2)
	}
}

// R10: Close while writes are still arriving: no worker runs after Close returned.
func TestCloseWithConcurrentWrites(t *testing.T) {
	st, b, _ := newStore(t)
	_ = b
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st.Learn(context.Background(), fmt.Sprintf("Note %d racing the close of the store.", i), "p", "", "")
		}(i)
	}
	time.Sleep(5 * time.Millisecond)
	st.Close()
	st.embedMu.Lock()
	run := st.embedRun
	st.embedMu.Unlock()
	wg.Wait()
	st.embedMu.Lock()
	run2 := st.embedRun
	st.embedMu.Unlock()
	t.Logf("worker running right after Close: %v; after the late writes: %v", run, run2)
	st.WaitEmbed()
	// a write after Close and after the last worker ended must not start another
	st.Learn(context.Background(), "A note written after the store was closed.", "p", "", "")
	st.embedMu.Lock()
	late := st.embedRun
	st.embedMu.Unlock()
	if late {
		t.Errorf("DEFECT a write after Close started an embedding worker")
	}
}

// R11: a slow but working embedder and a stream of writes: WaitEmbed (and so Close) returns in bounded time.
type slowBag struct{ bag }

func (s *slowBag) Docs(ctx context.Context, t []string) ([][]float32, error) {
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.bag.Docs(ctx, t)
}

func TestWaitEmbedUnderAStreamOfWrites(t *testing.T) {
	st, _, _ := newStore(t)
	st.Emb = &slowBag{}
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			st.Learn(context.Background(), fmt.Sprintf("Streaming note %d while someone waits for embedding.", i), "p", "", "")
			time.Sleep(50 * time.Millisecond)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	ch := make(chan struct{})
	go func() { st.WaitEmbed(); close(ch) }()
	select {
	case <-ch:
		t.Logf("WaitEmbed returned after %v under a stream of writes", time.Since(start))
	case <-time.After(WaitEmbedMax + 2*time.Second):
		t.Errorf("DEFECT WaitEmbed (and Close) did not return in %v while writes keep arriving", WaitEmbedMax)
	}
	close(stop)
}

// --- tests added with the fixes -------------------------------------------------------------------------------

// Sweep after a crash: a note whose deletion was left on disk and the accented page it was merged into go into one
// commit; nothing stays loose and the commit holds both.
func TestSweepCommitsDeletionAndAccentedPageTogether(t *testing.T) {
	st, _, repo := newStore(t)
	id, _ := st.Learn(context.Background(), "A note that is merged into an accented page.", "p", "", "")
	st.Commit.Flush()
	os.Remove(filepath.Join(repo, "notes", "p", id+".md")) // the crash left the deletion ...
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Sauvegarde nocturne é.md"), []byte("# S\n\nA note that is merged into an accented page.\n"), 0o644) // ... and the page
	before := strings.TrimSpace(gitOut(t, repo, "rev-list", "--count", "HEAD"))
	st.Commit.Sweep()
	st.Commit.Flush()
	if s := strings.TrimSpace(gitOut(t, repo, "status", "--porcelain", "-uall")); s != "" {
		t.Errorf("DEFECT left loose: %q", s)
	}
	after := strings.TrimSpace(gitOut(t, repo, "rev-list", "--count", "HEAD"))
	if after != fmt.Sprint(atoi(before)+1) {
		t.Errorf("commits %s -> %s, want one commit for the whole leftover set", before, after)
	}
	files := gitOut(t, repo, "-c", "core.quotepath=false", "show", "--name-status", "--format=", "HEAD")
	if !strings.Contains(files, "Sauvegarde nocturne é.md") || !strings.Contains(files, "D\tnotes/p/"+id+".md") {
		t.Errorf("the commit does not hold both:\n%s", files)
	}
}

func atoi(s string) int { var n int; fmt.Sscan(s, &n); return n }

// Sweep is all or nothing: when a page of the leftover set cannot be added, the deletion is not committed.
func TestSweepCommitsNothingWhenAPageCannotBeAdded(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	st, _, repo := newStore(t)
	id, _ := st.Learn(context.Background(), "A note whose page cannot be added.", "p", "", "")
	st.Commit.Flush()
	os.Remove(filepath.Join(repo, "notes", "p", id+".md"))
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	page := filepath.Join(repo, "wiki", "p", "Café.md")
	os.WriteFile(page, []byte("# Café\n\nA note whose page cannot be added.\n"), 0o000)
	defer os.Chmod(page, 0o644)
	before := strings.TrimSpace(gitOut(t, repo, "rev-list", "--count", "HEAD"))
	st.Commit.Sweep()
	st.Commit.Flush()
	if after := strings.TrimSpace(gitOut(t, repo, "rev-list", "--count", "HEAD")); after != before {
		t.Errorf("DEFECT the deletion was committed without its page (%s -> %s commits)", before, after)
	}
	os.Chmod(page, 0o644) // readable again: the next attempt commits both
	st.Commit.Flush()
	if s := strings.TrimSpace(gitOut(t, repo, "status", "--porcelain", "-uall")); s != "" {
		t.Errorf("after the page is readable: %q", s)
	}
}
