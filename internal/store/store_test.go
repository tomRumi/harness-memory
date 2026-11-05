package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/index"
)

type bag struct {
	fail atomic.Bool
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func (b *bag) vec(text string) []float32 {
	v := make([]float32, 32)
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%32]++
	}
	var s float64
	for _, x := range v {
		s += float64(x * x)
	}
	if s == 0 {
		v[0], s = 1, 1
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(s))
	}
	return v
}
func (b *bag) Model() string { return "bag" }
func (b *bag) ID() string    { return "bag" }
func (b *bag) Docs(ctx context.Context, t []string) ([][]float32, error) {
	if b.fail.Load() {
		return nil, errors.New("embedder down")
	}
	o := make([][]float32, len(t))
	for i, x := range t {
		o[i] = b.vec(x)
	}
	return o, nil
}
func (b *bag) Query(ctx context.Context, t string) ([]float32, error) {
	if b.fail.Load() {
		return nil, errors.New("embedder down")
	}
	return b.vec(t), nil
}

func newStore(t *testing.T) (*Store, *bag, string) {
	t.Helper()
	home := t.TempDir()
	repo := filepath.Join(home, "memory")
	for _, d := range []string{"wiki", "notes", "facts"} {
		os.MkdirAll(filepath.Join(repo, d), 0o755)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	db, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &bag{}
	st, err := New(repo, db, e, NewCommitter(repo, 50*time.Millisecond), filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, e, repo
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			n++
		}
		return nil
	})
	return n
}

func TestDetIDIsStableAndLooksLikeAUUID(t *testing.T) {
	a, b := DetID("mem", "hello"), DetID("mem", "hello")
	if a != b || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("%s %s", a, b)
	}
	if DetID("mem", "hello") == DetID("fact", "hello") || DetID("mem", "a") == DetID("mem", "b") {
		t.Fatal("ids must differ by prefix and text")
	}
}

func TestLearnIsIdempotentAndWritesAReadableNote(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	id1, err := st.Learn(ctx, "  The daemon keeps its state in one directory.  ", "proj", "rag:gotcha", "")
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := st.Learn(ctx, "The daemon keeps its state in one directory.", "proj", "rag:gotcha", "")
	if id1 != id2 || countFiles(t, filepath.Join(repo, "notes")) != 1 {
		t.Fatalf("not idempotent: %s %s, %d files", id1, id2, countFiles(t, filepath.Join(repo, "notes")))
	}
	raw, _ := os.ReadFile(filepath.Join(repo, "notes", "proj", id1+".md"))
	for _, want := range []string{"kind: rag:gotcha", "created:", "The daemon keeps its state"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("note file lacks %q:\n%s", want, raw)
		}
	}
	// it is searchable at once, and has a vector
	hits, _ := st.DB.Keyword("daemon state directory", index.Filter{Kinds: []string{"note"}})
	if len(hits) != 1 {
		t.Fatalf("not indexed: %+v", hits)
	}
	if miss, total, _ := st.DB.Pending("bag"); miss != 0 || total != 1 {
		t.Fatalf("not embedded: %d of %d", miss, total)
	}
	// empty text and a bad project name are refused
	if _, err := st.Learn(ctx, "   ", "proj", "x", ""); err == nil {
		t.Fatal("empty learning accepted")
	}
	if _, err := st.Learn(ctx, "text", "../evil", "x", ""); err == nil {
		t.Fatal("path-like project accepted")
	}
	// long text is cut at 500 characters
	long := strings.Repeat("word ", 200)
	id3, _ := st.Learn(ctx, long, "proj", "x", "")
	raw, _ = os.ReadFile(filepath.Join(repo, "notes", "proj", id3+".md"))
	if n := len([]rune(strings.TrimSpace(strings.SplitN(string(raw), "---\n", 3)[2]))); n > MaxNote {
		t.Fatalf("note body is %d characters", n)
	}
}

func TestLearnSucceedsWhileTheEmbedderIsDown(t *testing.T) {
	st, e, repo := newStore(t)
	e.fail.Store(true)
	id, err := st.Learn(context.Background(), "A note about the glimmerpost relay while the model server is down.", "p", "x", "")
	if err != nil {
		t.Fatalf("a write must not fail because of the embedder: %v", err)
	}
	if n := countFiles(t, filepath.Join(repo, "notes")); n != 1 {
		t.Fatalf("%d files written", n)
	}
	if st.EmbedError() == nil {
		t.Fatal("the embedding error must be kept for stats")
	}
	if hits, _ := st.DB.Keyword("glimmerpost relay", index.Filter{Kinds: []string{"note"}}); len(hits) != 1 {
		t.Fatalf("the note must be searchable by keyword: %+v", hits)
	}
	if _, err := st.AddFact(context.Background(), "A fact written while the model server is down.", "p", "", 0); err != nil {
		t.Fatalf("fact_add while down: %v", err)
	}
	st.Commit.Flush()
	if out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the notes were not committed while the embedder was down:\n%s", out)
	}
	// the embedder comes back: the next write embeds everything that is missing
	e.fail.Store(false)
	if _, err := st.Learn(context.Background(), "A second note about the glimmerpost relay.", "p", "x", ""); err != nil {
		t.Fatal(err)
	}
	if st.EmbedError() != nil {
		t.Fatalf("error not cleared: %v", st.EmbedError())
	}
	if miss, _, _ := st.DB.Pending("bag"); miss != 0 {
		t.Fatalf("%d chunk texts still without a vector (note %s)", miss, id)
	}
}

func TestHundredConcurrentWritesLoseNothing(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.Learn(ctx, fmt.Sprintf("Concurrent learning number %d about the widget subsystem.", i), "conc", "load:test", "")
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("write failed: %v", err)
	}
	if n := countFiles(t, filepath.Join(repo, "notes", "conc")); n != 100 {
		t.Fatalf("%d of 100 notes on disk", n)
	}
	var nodes int
	st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE kind='note' AND project='conc'`).Scan(&nodes)
	if nodes != 100 {
		t.Fatalf("%d of 100 notes in the index", nodes)
	}
	st.Commit.Flush()
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("uncommitted files remain (committer error: %v, commits %d):\n%.200s", st.Commit.LastErr(), st.Commit.Commits(), out)
	}
	log, _ := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").CombinedOutput()
	t.Logf("100 notes written concurrently, 0 errors, %s commit(s) (committer counted %d)", strings.TrimSpace(string(log)), st.Commit.Commits())
	if st.Commit.Commits() < 1 {
		t.Fatal("no commit was made")
	}
}

func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func hasNode(hits []index.Hit, id string) bool {
	for _, h := range hits {
		if strings.HasSuffix(h.NodeID, id) {
			return true
		}
	}
	return false
}

func TestRetireDeletesTheFileAndTheDeletionIsCommitted(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "The service listens on port 8765 by default.", "s", "cfg:port", "")
	st.Commit.Flush()
	rel := "notes/s/" + id + ".md"
	if !strings.Contains(gitOut(t, repo, "ls-files"), rel) {
		t.Fatalf("the note was not committed first")
	}
	// unknown and too short ids change nothing
	for _, bad := range []string{"zzzzzzzz", "ab"} {
		out, err := st.Retire(ctx, bad, "x", "")
		if err != nil || !strings.Contains(out, "nothing changed") {
			t.Fatalf("Retire(%q) = %q, %v", bad, out, err)
		}
	}
	out, err := st.Retire(ctx, id[:8], "wrong after all", "s")
	if err != nil || !strings.HasPrefix(out, "deleted "+id[:8]) {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(repo, rel)); !os.IsNotExist(err) {
		t.Fatalf("the file must be gone: %v", err)
	}
	if hits, _ := st.DB.Keyword("port 8765 listens", index.Filter{}); hasNode(hits, id) {
		t.Fatalf("a deleted note is still returned: %+v", hits)
	}
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id=?`, "notes/s/"+id).Scan(&n)
	if n != 0 {
		t.Fatal("the node is still in the index")
	}
	if out, _ := st.Retire(ctx, id[:8], "again", "s"); !strings.Contains(out, "nothing changed") {
		t.Fatalf("a second retire must change nothing: %q", out)
	}
	st.Commit.Flush()
	stat := gitOut(t, repo, "log", "--stat", "--format=%s", "-1")
	if !strings.Contains(stat, rel) || !strings.Contains(stat, "deletion") {
		t.Fatalf("the last commit must show the deletion:\n%s", stat)
	}
	if gitOut(t, repo, "status", "--porcelain") != "" {
		t.Fatalf("uncommitted changes remain: %s", gitOut(t, repo, "status", "--porcelain"))
	}
	// the text is still in history
	if !strings.Contains(gitOut(t, repo, "log", "-p", "--all", "--", rel), "port 8765") {
		t.Fatal("git history must keep the text")
	}
}

func TestNoteWrittenAndRetiredBeforeAnyCommitLeavesNoError(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "A note that is deleted before the committer ever ran.", "q", "k", "")
	if _, err := st.Retire(ctx, id[:8], "", "q"); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if err := st.Commit.LastErr(); err != nil {
		t.Fatalf("committer error: %v", err)
	}
	if gitOut(t, repo, "status", "--porcelain") != "" {
		t.Fatal("working tree not clean")
	}
}

func TestSupersedesDeletesTheOlderNoteAndTheDeletionIsCommitted(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	old, _ := st.Learn(ctx, "The service listens on port 8000 by default.", "s", "cfg:port", "")
	st.Commit.Flush()
	oldRel := "notes/s/" + old + ".md"
	res, err := st.Learn(ctx, "The service listens on port 8765 by default.", "s", "cfg:port", old[:8])
	if err != nil || !strings.Contains(res, "replaced "+old[:8]) {
		t.Fatalf("%q %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(repo, oldRel)); !os.IsNotExist(err) {
		t.Fatalf("the older note must be deleted: %v", err)
	}
	if hits, _ := st.DB.Keyword("port 8000 listens", index.Filter{Kinds: []string{"note"}}); hasNode(hits, old) {
		t.Fatalf("a replaced note is still returned: %+v", hits)
	}
	newID := strings.Fields(res)[0]
	if hits, _ := st.DB.Keyword("port 8765 listens", index.Filter{Kinds: []string{"note"}}); !hasNode(hits, newID) {
		t.Fatalf("the new note is missing: %+v", hits)
	}
	st.Commit.Flush()
	stat := gitOut(t, repo, "log", "--stat", "--format=%s", "-1")
	if !strings.Contains(stat, oldRel) || !strings.Contains(stat, "deletion") || !strings.Contains(stat, "notes/s/"+newID) {
		t.Fatalf("one commit must show the new note and the deletion:\n%s", stat)
	}
	if gitOut(t, repo, "status", "--porcelain") != "" {
		t.Fatal("uncommitted changes remain")
	}
	// an ambiguous prefix changes nothing
	a, _ := st.Learn(ctx, "Alpha note about caching layers and expiry times.", "amb", "k", "")
	b, _ := st.Learn(ctx, "Beta note about caching layers and expiry times.", "amb", "k", "")
	common := 0
	for common < len(a) && common < len(b) && a[common] == b[common] {
		common++
	}
	if common >= 4 { // only meaningful when the two ids share a prefix of 4 or more characters
		out, _ := st.Retire(ctx, a[:common], "", "amb")
		if !strings.Contains(out, "nothing changed") {
			t.Fatalf("ambiguous prefix deleted something: %q", out)
		}
	}
	// feedback
	f1, _ := st.Learn(ctx, "A note to be rated several times over.", "fb", "k", "")
	st.Feedback(f1[:8], true, "fb", "s1")
	msg, _ := st.Feedback(f1[:8], false, "fb", "s2")
	if !strings.Contains(msg, "helpful 1, unhelpful 1") {
		t.Fatalf("%q", msg)
	}
}

func TestOldStatusKeysInANoteFileAreIgnored(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "notes", "old"), 0o755)
	os.WriteFile(filepath.Join(repo, "notes", "old", "aaaa1111-0000-0000-0000-000000000000.md"), []byte("---\nkind: k\nstatus: archived\nsuperseded_by: x\n---\n\nAn archived looking note about the wibblefrost gateway.\n"), 0o644)
	if _, err := st.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	if hits, _ := st.DB.Keyword("wibblefrost gateway", index.Filter{Kinds: []string{"note"}}); len(hits) != 1 {
		t.Fatalf("an old status key must not hide the note: %+v", hits)
	}
	out, err := st.Retire(context.Background(), "aaaa1111", "", "old")
	if err != nil || !strings.HasPrefix(out, "deleted ") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestCommitterWaitsOutAMergeAndKeepsTheFiles(t *testing.T) {
	st, _, repo := newStore(t)
	os.WriteFile(filepath.Join(repo, ".git", "MERGE_HEAD"), []byte("0000000000000000000000000000000000000000\n"), 0o644)
	if _, err := st.Learn(context.Background(), "A note written during a merge.", "m", "k", ""); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if st.Commit.Commits() != 0 {
		t.Fatal("committed in the middle of a merge")
	}
	if !st.Commit.Busy() {
		t.Fatal("Busy must see MERGE_HEAD")
	}
	os.Remove(filepath.Join(repo, ".git", "MERGE_HEAD"))
	st.Commit.Flush()
	if st.Commit.Commits() != 1 {
		t.Fatalf("expected the pending note to be committed once the merge is over, commits=%d", st.Commit.Commits())
	}
}

func TestWikiReadStaysInsideWiki(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "wiki", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "p", "a.md"), []byte("# A\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "secret.md"), []byte("outside"), 0o644)
	if got, err := st.WikiRead("p/a.md"); err != nil || got != "# A\n" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := st.WikiRead("wiki/p/a.md"); err != nil || got != "# A\n" {
		t.Fatalf("with the wiki/ prefix: %q %v", got, err)
	}
	for _, bad := range []string{"../secret.md", "p/../../secret.md", "/etc/passwd", "p/a.txt", "", "p/missing.md"} {
		if got, err := st.WikiRead(bad); err == nil || strings.Contains(got, "outside") {
			t.Fatalf("WikiRead(%q) = %q, %v", bad, got, err)
		}
	}
	os.Symlink(filepath.Join(repo, "secret.md"), filepath.Join(repo, "wiki", "p", "link.md"))
	if got, err := st.WikiRead("p/link.md"); err == nil || strings.Contains(got, "outside") {
		t.Fatalf("a symlink out of wiki/ was followed: %q %v", got, err)
	}
}

func TestCommitterNeverSweepsInTheOwnersStagedWork(t *testing.T) {
	st, _, repo := newStore(t)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("owner work\n"), 0o644)
	exec.Command("git", "-C", repo, "add", "README.md").Run()
	if _, err := st.Learn(context.Background(), "A note while the owner has something staged.", "o", "k", ""); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if st.Commit.Commits() != 0 || st.Commit.LastErr() == nil || !strings.Contains(st.Commit.LastErr().Error(), "README.md") {
		t.Fatalf("commits %d, err %v", st.Commit.Commits(), st.Commit.LastErr())
	}
	out, _ := exec.Command("git", "-C", repo, "diff", "--cached", "--name-only").CombinedOutput()
	if !strings.Contains(string(out), "README.md") {
		t.Fatal("the owner's staged file was disturbed")
	}
	exec.Command("git", "-C", repo, "rm", "--cached", "-q", "-f", "README.md").Run() // works before the first commit, unlike reset
	os.Remove(filepath.Join(repo, "README.md"))
	st.Commit.Flush()
	if st.Commit.Commits() != 1 {
		t.Fatalf("after the owner's change is gone the note must be committed: %d, %v", st.Commit.Commits(), st.Commit.LastErr())
	}
}

func TestFirstWriteIntoAFreshRepositoryIsCommitted(t *testing.T) {
	st, _, repo := newStore(t) // no facts/ content, no commits yet
	if _, err := st.Learn(context.Background(), "The very first note of a new memory repository.", "fresh", "k", ""); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if st.Commit.Commits() != 1 || st.Commit.LastErr() != nil {
		t.Fatalf("commits %d, err %v", st.Commit.Commits(), st.Commit.LastErr())
	}
	out, _ := exec.Command("git", "-C", repo, "log", "--format=%s").CombinedOutput()
	if !strings.Contains(string(out), "memory: ") {
		t.Fatalf("log: %s", out)
	}
}

func TestSupersedesIsTrimmedAndNeedsEnoughOfTheId(t *testing.T) {
	st, _, _ := newStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "The first note about the cache size being 64 megabytes.", "s", "k", "")
	for _, bad := range []string{" ", "9", id[:3]} {
		msg, err := st.Learn(ctx, "Another note "+bad+" about something else entirely, long enough.", "s", "k", bad)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(msg, "|| replaced ") {
			t.Fatalf("supersedes %q replaced a note: %s", bad, msg)
		}
	}
	var active int
	st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE kind='note' AND project='s'`).Scan(&active)
	if active != 4 {
		t.Fatalf("active %d, want 4 (nothing replaced)", active)
	}
	msg, _ := st.Learn(ctx, "The real replacement: the cache size is 128 megabytes.", "s", "k", " "+id[:8]+" ")
	if !strings.Contains(msg, "|| replaced ") {
		t.Fatalf("a padded but valid id must work: %s", msg)
	}
}

func TestImportedFactsCanBeRetiredByTheirShortId(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "facts", "global"), 0o755)
	os.WriteFile(filepath.Join(repo, "facts", "global", "00004.md"), []byte("---\nid: 00004\nstatus: active\n---\nThe imported fact about a thing worth keeping.\n"), 0o644)
	if _, err := st.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	msg, err := st.Retire(context.Background(), "00004", "wrong now", "")
	if err != nil || !strings.HasPrefix(msg, "deleted ") {
		t.Fatalf("%q %v", msg, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "facts", "global", "00004.md")); !os.IsNotExist(err) {
		t.Fatalf("the fact file must be gone: %v", err)
	}
}

func TestCrashLeftoversAreNeverCommittedAndFreshRepoWithoutFactsWorks(t *testing.T) {
	st, _, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "notes", "p"), 0o755)
	os.WriteFile(filepath.Join(repo, "notes", "p", ".tmp-123456"), []byte("half a note"), 0o644)
	if _, err := st.Learn(context.Background(), "A real note that must be committed without the leftover.", "p", "k", ""); err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if st.Commit.Commits() != 1 || st.Commit.LastErr() != nil {
		t.Fatalf("commits %d err %v", st.Commit.Commits(), st.Commit.LastErr())
	}
	out, _ := exec.Command("git", "-C", repo, "ls-files").CombinedOutput()
	if strings.Contains(string(out), ".tmp-") {
		t.Fatalf("a temporary file was committed:\n%s", out)
	}
}

func TestCommitterRetriesAfterAFailure(t *testing.T) {
	st, _, repo := newStore(t)
	st.Commit = NewCommitter(repo, 20*time.Millisecond)
	os.WriteFile(filepath.Join(repo, "owner.txt"), []byte("x"), 0o644)
	exec.Command("git", "-C", repo, "add", "owner.txt").Run()
	if _, err := st.Learn(context.Background(), "A note saved while the owner has a file staged in the repository.", "r", "k", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if st.Commit.LastErr() == nil || st.Commit.Commits() != 0 {
		t.Fatalf("expected a refusal first: %d %v", st.Commit.Commits(), st.Commit.LastErr())
	}
	for i := 0; i < 50; i++ { // the committer may hold git's index lock for a moment; retry until the unstage works
		if exec.Command("git", "-C", repo, "rm", "--cached", "-q", "-f", "owner.txt").Run() == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	deadline := time.Now().Add(10 * time.Second)
	for st.Commit.Commits() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if st.Commit.Commits() != 1 {
		t.Fatalf("no retry after the owner unstaged: %d, %v", st.Commit.Commits(), st.Commit.LastErr())
	}
}

func TestNoGitDirectoryIsReportedNotOk(t *testing.T) {
	c := NewCommitter(t.TempDir(), time.Millisecond)
	if _, _, _, enabled := c.Status(); enabled {
		t.Fatal("a directory without .git must report commits as off")
	}
}

func TestAFileChangedAfterACommitIsCommittedAgain(t *testing.T) {
	_, _, repo := newStore(t)
	c := NewCommitter(repo, time.Hour)
	f := filepath.Join(repo, "notes", "w", "a.md")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("one\n"), 0o644)
	c.Notify("notes/w/a.md")
	c.Flush()
	os.WriteFile(f, []byte("two\n"), 0o644)
	c.Notify("notes/w/a.md")
	c.Flush()
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" || c.Commits() != 2 {
		t.Fatalf("commits %d, status %q", c.Commits(), out)
	}
}

func TestRatingsCountSessionsNotCalls(t *testing.T) {
	st, _, _ := newStore(t)
	ctx := context.Background()
	id, _ := st.Learn(ctx, "A note that a looping agent keeps rating as unhelpful again and again.", "fbk", "k", "")
	node := "notes/fbk/" + id
	for i := 0; i < 20; i++ {
		st.Feedback(id[:8], false, "fbk", "loop")
	}
	if h, u := st.Ratings(node); h != 0 || u != 1 {
		t.Fatalf("20 ratings of one session must count once: helpful %d unhelpful %d", h, u)
	}
	if msg, _ := st.Feedback(id[:8], false, "fbk", "  "); !strings.Contains(msg, "needs a session") {
		t.Fatalf("a rating without a session must be refused: %s", msg)
	}
	if len(st.RetireCandidates()) != 0 {
		t.Fatal("one session is not enough to retire a note")
	}
	for i := 1; i < RetireAfter; i++ {
		st.Feedback(id[:8], false, "fbk", fmt.Sprintf("s%d", i))
	}
	if c := st.RetireCandidates(); len(c) != 1 || c[0] != node {
		t.Fatalf("5 distinct sessions must make a candidate: %v", c)
	}
	st.Feedback(id[:8], true, "fbk", "s1") // one session changes its mind: it now counts as helpful
	if c := st.RetireCandidates(); len(c) != 0 {
		t.Fatalf("a helpful rating blocks retirement: %v", c)
	}
	st.Feedback(id[:8], false, "fbk", "s1") // and back: the last rating of the session is what counts
	if c := st.RetireCandidates(); len(c) != 1 {
		t.Fatalf("last rating per session: %v", c)
	}
	if _, err := st.Retire(ctx, id[:8], "rated down", "fbk"); err != nil {
		t.Fatal(err)
	}
	if c := st.RetireCandidates(); len(c) != 0 {
		t.Fatalf("a deleted note is no candidate: %v", c)
	}
}
