package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory/internal/index"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// hung is an embedder that never answers until its context ends.
type hung struct{}

func (hung) Model() string { return "hung" }
func (hung) ID() string    { return "hung" }
func (hung) Docs(ctx context.Context, t []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (hung) Query(ctx context.Context, t string) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func hungStore(t *testing.T) (*Store, string) {
	st, _, repo := newStore(t)
	st.Emb = hung{}
	return st, repo
}

// Item 7: a hung embedder must not hold a save, and concurrent saves do not queue behind it.
func TestSavesReturnInMillisecondsWithAHungEmbedder(t *testing.T) {
	st, repo := hungStore(t)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := st.Learn(context.Background(), "A note number "+string(rune('a'+i))+" about the relay while the embedder hangs.", "p", "", ""); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("3 saves took %v with a hung embedder", d)
	}
	if _, err := st.AddFact(context.Background(), "A fact while the embedder hangs.", "p", "", 0); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, filepath.Join(repo, "notes")); n != 3 {
		t.Fatalf("%d notes", n)
	}
	st.Commit.Flush()
	if out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("not committed:\n%s", out)
	}
	begin := time.Now()
	st.WaitEmbed() // the worker gives up after EmbedTimeout
	if d := time.Since(begin); d > EmbedTimeout+3*time.Second {
		t.Fatalf("embedding ran %v", d)
	}
	if st.EmbedError() == nil {
		t.Fatal("the timeout must be kept as the embedding error")
	}
}

func TestEmbeddingRunsAfterTheWriteAndOnWriteFollows(t *testing.T) {
	st, _, _ := newStore(t)
	var mu sync.Mutex
	calls := 0
	st.OnWrite = func() { mu.Lock(); calls++; mu.Unlock() }
	if _, err := st.Learn(context.Background(), "A note about the glimmerpost relay.", "p", "", ""); err != nil {
		t.Fatal(err)
	}
	st.WaitEmbed()
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("OnWrite not called after embedding")
	}
	if miss, _, _ := st.DB.Pending("bag"); miss != 0 {
		t.Fatalf("%d chunks without vector", miss)
	}
}

// Item 6: Do restores every file and index entry when fn fails or panics.
func TestDoRestoresFilesAndIndexOnErrorAndPanic(t *testing.T) {
	st, _, repo := newStore(t)
	old := filepath.Join(repo, "wiki", "p", "Old.md")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("# Old\n\nThe original text of the old page.\n"), 0o644)
	if _, err := st.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	id, _ := st.Learn(context.Background(), "A note that a failing merge must not delete.", "p", "", "")
	st.WaitEmbed()
	st.Commit.Flush()
	before := gitState(t, repo)
	for name, fn := range map[string]func(tx *Tx) error{
		"error": func(tx *Tx) error {
			if err := tx.Write("wiki/p/Old.md", []byte("# Old\n\nchanged\n")); err != nil {
				return err
			}
			if err := tx.Write("wiki/p/New.md", []byte("# New\n\nnew page\n")); err != nil {
				return err
			}
			if err := tx.Delete("notes/p/"+id, "p", "", ""); err != nil {
				return err
			}
			return errors.New("boom")
		},
		"panic": func(tx *Tx) error {
			tx.Write("wiki/p/Old.md", []byte("# Old\n\nchanged\n"))
			tx.Write("wiki/p/New.md", []byte("# New\n\nnew page\n"))
			tx.Delete("notes/p/"+id, "p", "", "")
			panic("boom")
		},
	} {
		func() {
			defer func() {
				if r := recover(); (name == "panic") != (r != nil) {
					t.Fatalf("%s: recovered %v", name, r)
				}
			}()
			err := st.Do(fn)
			if name == "error" && (err == nil || !strings.Contains(err.Error(), "boom")) {
				t.Fatalf("error not returned: %v", err)
			}
		}()
		if got := gitState(t, repo); got != before {
			t.Fatalf("%s: files differ after rollback:\n%s\nwas:\n%s", name, got, before)
		}
		var n int
		st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id IN ('wiki/p/New','notes/p/` + id + `')`).Scan(&n)
		if n != 1 { // the note is back, New is not there
			t.Fatalf("%s: index has %d of the two nodes", name, n)
		}
		var sum int
		st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id='wiki/p/Old'`).Scan(&sum)
		if sum != 1 {
			t.Fatalf("%s: Old page gone from the index", name)
		}
		if hits, _ := st.DB.Keyword("original text of the old page", index.Filter{Kinds: []string{"wiki"}}); len(hits) != 1 {
			t.Fatalf("%s: the index does not hold the original text: %+v", name, hits)
		}
	}
	// nothing was announced to the committer
	st.Commit.Flush()
	if got := gitState(t, repo); got != before {
		t.Fatal("a failed Do reached git")
	}
	// the lock was released: another Do works
	if err := st.Do(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// gitState is the content of every file under notes, facts and wiki, plus git status.
func gitState(t *testing.T, repo string) string {
	var b strings.Builder
	for _, d := range []string{"notes", "facts", "wiki"} {
		filepath.WalkDir(filepath.Join(repo, d), func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				data, _ := os.ReadFile(p)
				b.WriteString(p + "\n" + string(data) + "\n")
			}
			return nil
		})
	}
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain", "-uall").CombinedOutput()
	return b.String() + string(out)
}

// Item 6: the sweep after a crash also commits wiki pages.
func TestSweepCoversWiki(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Page.md"), []byte("# Page\n\nText.\n"), 0o644)
	st.Commit.Sweep()
	st.Commit.Flush()
	if out := gitLog(t, repo, "ls-files"); !strings.Contains(out, "wiki/p/Page.md") {
		t.Fatalf("not committed: %q", out)
	}
}

// Item 4: a second store (another process in real life) waits for the lock file.
func TestWriteLockIsSharedThroughTheLockFile(t *testing.T) {
	st, _, repo := newStore(t)
	home := filepath.Dir(repo)
	db2, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	st2, err := New(repo, db2, nil, nil, filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	go st.Do(func(tx *Tx) error { close(entered); <-release; return nil })
	<-entered
	got := make(chan struct{})
	go func() { st2.Do(func(tx *Tx) error { close(got); return nil }) }()
	select {
	case <-got:
		t.Fatal("the second store entered while the first held the lock")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the second store never got the lock")
	}
	if _, err := os.Stat(filepath.Join(home, "write.lock")); err != nil {
		t.Fatal(err)
	}
}

// Item 4: every write path takes the lock file.
func TestEveryWritePathWaitsForTheLockFile(t *testing.T) {
	st, _, repo := newStore(t)
	home := filepath.Dir(repo)
	id, _ := st.Learn(context.Background(), "A note to retire through the locked path.", "p", "", "")
	unlock, err := lockFile(filepath.Join(home, "write.lock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, op := range map[string]func(){
		"learn":  func() { st.Learn(ctx, "Another note for the locked path.", "p", "", "") },
		"fact":   func() { st.AddFact(ctx, "A fact for the locked path.", "p", "", 0) },
		"retire": func() { st.Retire(ctx, id[:8], "", "p") },
		"do":     func() { st.Do(func(tx *Tx) error { return nil }) },
		"index":  func() { st.WriteIndexPage(ctx, "p") },
	} {
		done := make(chan struct{})
		go func() { op(); close(done) }()
		select {
		case <-done:
			t.Errorf("%s did not wait for the lock file", name)
		case <-time.After(200 * time.Millisecond):
		}
		unlock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s never finished", name)
		}
		if unlock, err = lockFile(filepath.Join(home, "write.lock")); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
}

// Item 13: a hand-written Index.md is left alone; a generated one is rewritten.
func TestIndexPageLeavesAHandWrittenIndexAlone(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "Ports.md"), []byte("# Ports\n\nThe ports.\n"), 0o644)
	idx := filepath.Join(repo, "wiki", "p", "Index.md")
	os.WriteFile(idx, []byte("# Mine\n\nHand-written.\n"), 0o644)
	st.DB.Rescan(repo)
	wrote, err := st.WriteIndexPage(context.Background(), "p")
	if wrote || !errors.Is(err, ErrHandWritten) || err.Error() != "Index.md is hand-written; left alone" {
		t.Fatalf("wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(idx); !strings.Contains(string(b), "Hand-written.") {
		t.Fatal("overwritten")
	}
	// a hand-written file that claims generated: false is also left alone
	os.WriteFile(idx, []byte("---\ngenerated: false\n---\n\nMine.\n"), 0o644)
	if _, err := st.WriteIndexPage(context.Background(), "p"); !errors.Is(err, ErrHandWritten) {
		t.Fatal(err)
	}
	// the generated one is regenerated
	os.WriteFile(idx, []byte("---\ntitle: Index\ngenerated: true\n---\n\nstale\n"), 0o644)
	if wrote, err := st.WriteIndexPage(context.Background(), "p"); err != nil || !wrote {
		t.Fatalf("wrote=%v err=%v", wrote, err)
	}
	if b, _ := os.ReadFile(idx); !strings.Contains(string(b), "[[Ports]]") {
		t.Fatalf("not regenerated: %s", b)
	}
	// none yet: created
	os.Remove(idx)
	if wrote, err := st.WriteIndexPage(context.Background(), "p"); err != nil || !wrote {
		t.Fatalf("wrote=%v err=%v", wrote, err)
	}
}

// Item 12: a text with no letter or digit is not a memory.
func TestLearnRefusesATextWithNoLetterOrDigit(t *testing.T) {
	st, _, repo := newStore(t)
	for _, text := range []string{"=> !!! ??? -> ...", "—", "  ...  ", "👍"} {
		if _, err := st.Learn(context.Background(), text, "p", "", ""); err == nil || !strings.Contains(err.Error(), "nothing to remember") {
			t.Errorf("%q: %v", text, err)
		}
	}
	if n := countFiles(t, filepath.Join(repo, "notes")); n != 0 {
		t.Fatalf("%d files", n)
	}
	if _, err := st.Learn(context.Background(), "=> 5 <=", "p", "", ""); err != nil {
		t.Fatalf("a digit is enough: %v", err)
	}
}

// Item 5: the one project rule lower-cases (wiki.Project); the store reads no project as global.
func TestProjectRuleLowerCasesAndNoProjectIsGlobal(t *testing.T) {
	if p, err := wiki.Project("PROJ"); err != nil || p != "proj" {
		t.Fatal(p, err)
	}
	if p, err := project(""); err != nil || p != GlobalProject {
		t.Fatal(p, err)
	}
	if _, err := wiki.Project(""); err == nil {
		t.Fatal("an empty name is not a project")
	}
	if _, err := wiki.Project("../x"); err == nil {
		t.Fatal("path accepted")
	}
}

// Item 15: no commit on a detached HEAD; an unborn branch is fine.
func TestCommitterRefusesOnADetachedHead(t *testing.T) {
	st, _, repo := newStore(t)
	c := st.Commit
	if c.Detached() {
		t.Fatal("an unborn branch is not detached")
	}
	st.Learn(context.Background(), "The first note, committed on the unborn branch.", "p", "", "")
	c.Flush()
	if gitLog(t, repo, "rev-list", "--count", "HEAD") != "1" {
		t.Fatal("first commit")
	}
	gitLog(t, repo, "checkout", "-q", "--detach")
	if !c.Detached() {
		t.Fatal("not detected")
	}
	st.Learn(context.Background(), "A second note written while HEAD is detached.", "p", "", "")
	c.Flush()
	if gitLog(t, repo, "rev-list", "--count", "HEAD") != "1" {
		t.Fatal("committed on a detached HEAD")
	}
	if out := gitLog(t, repo, "status", "--porcelain"); !strings.Contains(out, "notes/p/") {
		t.Fatalf("the note must stay on disk, uncommitted: %q", out)
	}
	gitLog(t, repo, "checkout", "-q", "main")
	c.Flush()
	if gitLog(t, repo, "rev-list", "--count", "HEAD") != "2" {
		t.Fatalf("not committed after the branch is back: %s", gitLog(t, repo, "log", "--oneline"))
	}
}

// Item 19: the messages of several Notify calls are all kept; the whole is capped.
func TestCommitMessagesAreCapped(t *testing.T) {
	st, _, repo := newStore(t)
	os.WriteFile(filepath.Join(repo, "notes", "a.md"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "notes", "b.md"), []byte("b\n"), 0o644)
	st.Commit.NotifyMsg("subject one", "notes/a.md")
	st.Commit.NotifyMsg("big body "+strings.Repeat("x", 5000), "notes/b.md")
	st.Commit.Flush()
	if got := gitLog(t, repo, "log", "--format=%s"); got != "subject one" {
		t.Fatalf("subject %q", got)
	}
	if body := gitLog(t, repo, "log", "--format=%B"); len([]rune(body)) > MaxMessage+5 || !strings.Contains(body, "big body") {
		t.Fatalf("body of %d characters", len(body))
	}
}
