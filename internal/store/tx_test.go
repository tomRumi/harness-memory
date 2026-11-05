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

func gitLog(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCommitterKeepsEveryMessageSubjectFirst(t *testing.T) {
	st, _, repo := newStore(t)
	c := st.Commit
	os.WriteFile(filepath.Join(repo, "notes", "a.md"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "notes", "b.md"), []byte("b\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "notes", "c.md"), []byte("c\n"), 0o644)
	c.NotifyMsg("first message", "notes/a.md")
	c.NotifyMsg("second message", "notes/b.md")
	c.Notify("notes/c.md") // no message: the earlier ones stay
	c.Flush()
	if got := gitLog(t, repo, "log", "--format=%s"); got != "first message" {
		t.Fatalf("subject %q", got)
	}
	if body := gitLog(t, repo, "log", "--format=%b"); !strings.Contains(body, "second message") {
		t.Fatalf("the second message is lost: %q", body)
	}
	if gitLog(t, repo, "rev-list", "--count", "HEAD") != "1" {
		t.Fatal("one commit expected")
	}
	// the message is used once; the next commit has the default
	os.WriteFile(filepath.Join(repo, "notes", "d.md"), []byte("d\n"), 0o644)
	c.Notify("notes/d.md")
	c.Flush()
	if got := gitLog(t, repo, "log", "-1", "--format=%s"); got != "memory: 1 change(s)" {
		t.Fatalf("default subject %q", got)
	}
}

func TestTxWritesDeletesAndCommitsOnceUnderTheLock(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	id, err := st.Learn(ctx, "A note that will be merged into a page by the transaction.", "p", "learning", "")
	if err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()

	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.Do(func(tx *Tx) error {
			close(entered)
			<-release
			if err := tx.Write("wiki/p/Page.md", []byte("---\nsummary: s\n---\nBody.\n")); err != nil {
				return err
			}
			if err := tx.Delete("notes/p/"+id, "p", "", ""); err != nil {
				return err
			}
			if err := tx.IndexPage("p"); err != nil {
				return err
			}
			if got := strings.Join(tx.Changed(), ","); got != "wiki/p/Page.md,notes/p/"+id+".md,wiki/p/Index.md" {
				t.Errorf("changed %s", got)
			}
			tx.Finish(ctx, "tx message")
			return nil
		})
	}()
	<-entered
	learned := make(chan struct{})
	go func() {
		st.Learn(ctx, "A second note written while the transaction holds the lock.", "p", "learning", "")
		close(learned)
	}()
	select {
	case <-learned:
		t.Fatal("a write got in while the transaction held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-learned
	// the page, the deletion and the Index are in the index at once
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id IN ('wiki/p/Page','wiki/p/Index')`).Scan(&n)
	var gone int
	st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id=?`, "notes/p/"+id).Scan(&gone)
	if n != 2 || gone != 0 {
		t.Fatalf("index: %d pages, %d deleted notes still there", n, gone)
	}
	st.Commit.Flush()
	subjects := gitLog(t, repo, "log", "--format=%s", "-3")
	if !strings.Contains(subjects, "tx message") {
		t.Fatalf("subjects:\n%s", subjects)
	}
	show := gitLog(t, repo, "show", "--name-status", "--format=%s", "--diff-filter=D", "--grep=tx message", "-1")
	if !strings.Contains(show, "D\tnotes/p/"+id+".md") {
		t.Fatalf("the deletion is not in the transaction's commit:\n%s", show)
	}
}

func TestTxFinishWithNothingChangedTellsNobody(t *testing.T) {
	st, _, repo := newStore(t)
	if err := st.Do(func(tx *Tx) error { tx.Finish(context.Background(), "nothing"); return nil }); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if out, _ := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output(); strings.TrimSpace(string(out)) != "" && strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("a commit was made: %s", out)
	}
}
