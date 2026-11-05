package curate

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

// bag is a fake embedder: a hashed bag of words, so texts that share words are close.
type bag struct{ fail atomic.Bool }

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

const proj = "proj"

// newStore makes a store on a throwaway git repository. The clock of the merge is fixed.
func newStore(t *testing.T) (*store.Store, string) {
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
	st, err := store.New(repo, db, &bag{}, store.NewCommitter(repo, 20*time.Millisecond), filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	old := Now
	Now = func() time.Time { return time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { Now = old })
	return st, repo
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// commits flushes the committer and counts the commits.
func commits(t *testing.T, st *store.Store, repo string) int {
	t.Helper()
	st.Commit.Flush()
	out, err := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		return 0 // no commit yet
	}
	var n int
	fmt.Sscan(string(out), &n)
	return n
}

// learnN stores n notes of a project and returns them in creation order.
func learnN(t *testing.T, st *store.Store, project string, n int, tmpl string) []Note {
	t.Helper()
	var out []Note
	for i := 0; i < n; i++ {
		text := fmt.Sprintf(tmpl, i, 10+i)
		id, err := st.Learn(context.Background(), text, project, "learning", "")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, Note{ID: id, Text: text})
		time.Sleep(2 * time.Millisecond) // distinct created times
	}
	return out
}

const tmplWorker = "Worker %d restarts after %d failed health checks and then pages the on-call owner."

func ids(ns []Note) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

// putFile writes a repository file the way a hand edit does and lets the index see it.
func putFile(t *testing.T, st *store.Store, rel, content string) {
	t.Helper()
	p := filepath.Join(st.Repo, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Upsert(st.Repo, rel); err != nil {
		t.Fatal(err)
	}
	st.Commit.Notify(rel)
	if _, err := st.DB.EmbedPending(context.Background(), st.Emb, nil); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, st *store.Store, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(st.Repo, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(st *store.Store, rel string) bool {
	_, err := os.Stat(filepath.Join(st.Repo, filepath.FromSlash(rel)))
	return err == nil
}

func noteCount(t *testing.T, st *store.Store, project string) int {
	t.Helper()
	ns, err := readNotes(st.Repo, project)
	if err != nil {
		t.Fatal(err)
	}
	return len(ns)
}

func bullets(ns []Note) string {
	var b strings.Builder
	for _, n := range ns {
		b.WriteString("- " + n.Text + "\n")
	}
	return b.String()
}

func fmOf(t *testing.T, raw string) md.Doc {
	t.Helper()
	d := md.Parse([]byte(raw))
	if d.Error != "" {
		t.Fatalf("frontmatter: %s\n%s", d.Error, raw)
	}
	return d
}
