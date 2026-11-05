package curate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/gate"
	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

type fakeMerger struct {
	mode  string // "faithful", "lossy", "error"
	calls int
}

func (f *fakeMerger) Section(ctx context.Context, existing string, notes []gate.Note) (string, error) {
	f.calls++
	switch f.mode {
	case "error":
		return "", fmt.Errorf("model server down")
	case "lossy":
		var b strings.Builder
		for _, n := range notes {
			b.WriteString("- something about it [[" + n.ID + "]]\n")
		}
		return b.String(), nil
	}
	return Fallback(notes), nil // a faithful answer
}

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	home := t.TempDir()
	repo := filepath.Join(home, "memory")
	for _, d := range []string{"wiki", "notes", "facts"} {
		os.MkdirAll(filepath.Join(repo, d), 0o755)
	}
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
		exec.Command("git", append([]string{"-C", repo}, a...)...).Run()
	}
	db, err := index.Open(filepath.Join(home, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.New(repo, db, nil, store.NewCommitter(repo, 30*time.Millisecond), filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, repo
}

func learnN(t *testing.T, st *store.Store, n int, proj, kind string) []string {
	var ids []string
	for i := 0; i < n; i++ {
		id, err := st.Learn(context.Background(), fmt.Sprintf("Fact number %d of the %s area: the job %s runs every %d minutes and keeps %d copies.", i, kind, kind, 5+i, 3+i), proj, kind, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond) // distinct creation times
	}
	return ids
}

func owner() Config { var c Config; c.Curation.Owner = true; return c }

func commits(repo string) int {
	out, _ := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output()
	var n int
	fmt.Sscan(strings.TrimSpace(string(out)), &n)
	return n
}

func count(st *store.Store, where string) int {
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM nodes WHERE kind='note' AND ` + where).Scan(&n)
	return n
}

func TestDryRunChangesNothing(t *testing.T) {
	st, repo := newStore(t)
	learnN(t, st, 12, "p", "rag:gotcha")
	learnN(t, st, 3, "p", "ops:x")
	learnN(t, st, 2, "p", "nocolon")
	st.Commit.Flush()
	before := commits(repo)
	rep, err := Run(context.Background(), st, &fakeMerger{}, owner(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outcomes) != 1 || rep.Outcomes[0].Result != "planned" || len(rep.Outcomes[0].IDs) != 10 || rep.Manual != 2 || rep.Waiting != 2+3 {
		t.Fatalf("%+v", rep)
	}
	st.Commit.Flush()
	if _, err := os.Stat(filepath.Join(repo, "wiki/p/modules/rag.md")); err == nil || commits(repo) != before || count(st, "status='active'") != 17 {
		t.Fatal("a dry run changed something")
	}
}

func TestTenNotesBecomeOneEntryInOneCommit(t *testing.T) {
	st, repo := newStore(t)
	learnN(t, st, 12, "p", "rag:gotcha")
	st.Commit.Flush()
	before := commits(repo)
	rep, err := Run(context.Background(), st, &fakeMerger{}, owner(), true, "")
	if err != nil {
		t.Fatal(err)
	}
	st.Commit.Flush()
	if rep.Merged() != 10 || rep.Outcomes[0].Result != "scaffold" {
		t.Fatalf("%s", rep.Text())
	}
	if got := commits(repo) - before; got != 1 {
		t.Fatalf("expected one commit for the whole merge, got %d", got)
	}
	b, err := os.ReadFile(filepath.Join(repo, "wiki/p/modules/rag.md"))
	if err != nil || strings.Count(string(b), "[[") != 10 {
		t.Fatalf("entry: %v\n%s", err, b)
	}
	if count(st, "status='superseded' AND superseded_by='wiki/p/modules/rag'") != 10 || count(st, "status='active'") != 2 {
		t.Fatal("ten notes must be superseded by the entry and two must stay active")
	}
	if st.Commit.LastErr() != nil {
		t.Fatal(st.Commit.LastErr())
	}
	// the notes are still on disk
	files, _ := filepath.Glob(filepath.Join(repo, "notes/p/*.md"))
	if len(files) != 12 {
		t.Fatalf("notes deleted: %d files", len(files))
	}
	// a second run has nothing to do
	rep, _ = Run(context.Background(), st, &fakeMerger{}, owner(), true, "")
	if rep.Merged() != 0 || rep.Waiting != 2 {
		t.Fatalf("%s", rep.Text())
	}
}

func TestExistingEntryGetsAnAppendedSectionAndTextIsKept(t *testing.T) {
	st, repo := newStore(t)
	os.MkdirAll(filepath.Join(repo, "wiki/p/modules"), 0o755)
	orig := "# rag\n\n## Setup\nThe daemon listens on port 8765.\n"
	os.WriteFile(filepath.Join(repo, "wiki/p/modules/rag.md"), []byte(orig), 0o644)
	if _, err := st.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	learnN(t, st, 10, "p", "rag:gotcha")
	m := &fakeMerger{mode: "faithful"}
	rep, err := Run(context.Background(), st, m, owner(), true, "")
	if err != nil || rep.Outcomes[0].Result != "merged" || m.calls != 1 {
		t.Fatalf("%v %s", err, rep.Text())
	}
	b, _ := os.ReadFile(filepath.Join(repo, "wiki/p/modules/rag.md"))
	if !strings.HasPrefix(string(b), orig[:len(orig)-1]) || !strings.Contains(string(b), "## Consolidated learnings (") {
		t.Fatalf("existing text must be untouched and a section appended:\n%s", b)
	}
}

func TestLossyModelAndModelErrorFallBackToBullets(t *testing.T) {
	for _, mode := range []string{"lossy", "error"} {
		st, repo := newStore(t)
		os.MkdirAll(filepath.Join(repo, "wiki/p/modules"), 0o755)
		os.WriteFile(filepath.Join(repo, "wiki/p/modules/rag.md"), []byte("# rag\n\n## Setup\nText.\n"), 0o644)
		st.DB.Rescan(repo)
		learnN(t, st, 10, "p", "rag:gotcha")
		rep, err := Run(context.Background(), st, &fakeMerger{mode: mode}, owner(), true, "")
		if err != nil || rep.Outcomes[0].Result != "fallback" || len(rep.Outcomes[0].Problems) == 0 {
			t.Fatalf("%s: %v %s", mode, err, rep.Text())
		}
		b, _ := os.ReadFile(filepath.Join(repo, "wiki/p/modules/rag.md"))
		if strings.Count(string(b), "[[") != 10 || !strings.Contains(string(b), "every 5 minutes and keeps 3 copies") {
			t.Fatalf("%s: the fallback must hold every note whole:\n%s", mode, b)
		}
		if count(st, "status='superseded'") != 10 {
			t.Fatalf("%s: notes must be superseded after the fallback", mode)
		}
	}
}

func TestOnlyTheOwnerMerges(t *testing.T) {
	st, _ := newStore(t)
	learnN(t, st, 10, "p", "rag:gotcha")
	rep, err := Run(context.Background(), st, &fakeMerger{}, Config{}, true, "")
	if err != nil || rep.Merged() != 0 || rep.Skipped == "" || count(st, "status='active'") != 10 {
		t.Fatalf("%v %s", err, rep.Text())
	}
}

func TestNothingIsMergedDuringARebase(t *testing.T) {
	st, repo := newStore(t)
	learnN(t, st, 10, "p", "rag:gotcha")
	st.Commit.Flush()
	os.WriteFile(filepath.Join(repo, ".git", "MERGE_HEAD"), []byte("x"), 0o644)
	rep, err := Run(context.Background(), st, &fakeMerger{}, owner(), true, "")
	if err != nil || rep.Merged() != 0 || !strings.Contains(rep.Skipped, "merge or rebase") || count(st, "status='active'") != 10 {
		t.Fatalf("%v %s", err, rep.Text())
	}
}

func TestFiveBadSessionsRetireANoteAndARestoreBringsItBack(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	ids := learnN(t, st, 1, "p", "misc:thing")
	for i := 0; i < store.RetireAfter; i++ {
		st.Feedback(ids[0][:8], false, "p", fmt.Sprintf("s%d", i))
	}
	rep, err := Run(ctx, st, &fakeMerger{}, owner(), true, "")
	if err != nil || len(rep.Retired) != 1 || count(st, "status='retired'") != 1 {
		t.Fatalf("%v %s", err, rep.Text())
	}
	msg, err := st.Restore(ctx, ids[0][:8], "p")
	if err != nil || !strings.Contains(msg, "restored") || count(st, "status='active'") != 1 {
		t.Fatalf("%q %v", msg, err)
	}
	// the old ratings no longer count, so the next run leaves the note alone
	rep, _ = Run(ctx, st, &fakeMerger{}, owner(), true, "")
	if len(rep.Retired) != 0 || count(st, "status='active'") != 1 {
		t.Fatalf("the note was retired again from ratings given before the restore: %s", rep.Text())
	}
}

func TestLLMClientSpeaksChatCompletions(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"choices":[{"message":{"content":"- text [[abc]]"}}]}`))
	}))
	defer srv.Close()
	l := &LLM{URL: srv.URL + "/v1", Model: "m"}
	out, err := l.Section(context.Background(), "# file", []gate.Note{{ID: "abc", Text: "a note"}})
	if err != nil || out != "- text [[abc]]" || got["model"] != "m" {
		t.Fatalf("%q %v %v", out, err, got)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	if _, err := (&LLM{URL: bad.URL, Model: "m"}).Section(context.Background(), "", nil); err == nil {
		t.Fatal("a server error must be an error")
	}
}

func TestConfigDefaultsToNotTheOwner(t *testing.T) {
	d := t.TempDir()
	if c, err := LoadConfig(d); err != nil || c.Curation.Owner {
		t.Fatalf("%v %+v", err, c)
	}
	os.WriteFile(filepath.Join(d, "config.yaml"), []byte("curation:\n  owner: true\n"), 0o644)
	if c, err := LoadConfig(d); err != nil || !c.Curation.Owner {
		t.Fatalf("%v %+v", err, c)
	}
}
