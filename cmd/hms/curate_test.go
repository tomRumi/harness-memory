package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

var noModel = []string{"HMS_LLM_URL=", "HMS_LLM_MODEL=", "HMS_LLM_KEY="}

// curateFixture makes a home with three waiting notes in project "proj" and one in "other", all committed.
func curateFixture(t *testing.T) (home, repo string, ids []string) {
	t.Helper()
	up := fakeOllama(t)
	home = t.TempDir()
	if _, _, c := hms(t, home, up.URL, nil, "", "init"); c != 0 {
		t.Fatal("init failed")
	}
	repo = filepath.Join(home, "memory")
	put := func(project, id, created, text string) {
		p := filepath.Join(repo, "notes", project, id+".md")
		os.MkdirAll(filepath.Dir(p), 0o755)
		body := "---\nkind: learning\ncreated: " + created + "\nsource: learn\n---\n\n" + text + "\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("proj", "11111111-aaaa", "2020-01-01T10:00:00Z", "The nightly job restarts the worker after 12 failed health checks and then pages the owner.")
	put("proj", "22222222-bbbb", "2020-01-02T10:00:00Z", "Backups run at 02:30 and keep 7 copies in the archive folder on the second disk.")
	put("proj", "33333333-cccc", "2020-01-03T10:00:00Z", "Never commit secrets.")
	put("other", "44444444-dddd", "2020-01-04T10:00:00Z", "A note of another project that is waiting to be merged somewhere.")
	for _, args := range [][]string{{"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}, {"add", "-A"}, {"commit", "-q", "-m", "seed"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return home, repo, []string{"11111111-aaaa", "22222222-bbbb", "33333333-cccc", "44444444-dddd"}
}

func gitIn(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func TestCurateNoFlagsListsWaitingNotesReadOnly(t *testing.T) {
	home, repo, _ := curateFixture(t)
	before := gitIn(t, repo, "status", "--porcelain")
	out, errOut, c := hms(t, home, deadURL(), noModel, "", "curate")
	if c != 0 || errOut != "" {
		t.Fatalf("%d %q %q", c, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "other: 1 notes waiting, oldest ") || !strings.HasPrefix(lines[1], "proj: 3 notes waiting, oldest ") || !strings.HasSuffix(lines[1], " d") {
		t.Fatalf("output:\n%s", out)
	}
	if gitIn(t, repo, "status", "--porcelain") != before {
		t.Fatal("hms curate changed the repository")
	}
	empty := t.TempDir()
	hms(t, empty, deadURL(), nil, "", "init")
	if out, _, c := hms(t, empty, deadURL(), noModel, "", "curate"); c != 0 || out != "no notes waiting\n" {
		t.Fatalf("%d %q", c, out)
	}
}

func TestCurateUsageErrors(t *testing.T) {
	home, _, _ := curateFixture(t)
	if _, e, c := hms(t, home, deadURL(), noModel, "", "curate", "-pending", "-apply"); c != 2 || !strings.Contains(e, "usage") {
		t.Fatalf("two modes: %d %q", c, e)
	}
	if _, _, c := hms(t, home, deadURL(), noModel, "", "curate", "-nonsense"); c != 2 {
		t.Fatalf("unknown flag: %d", c)
	}
}

func TestCuratePendingPrintsIndentedJSON(t *testing.T) {
	home, _, ids := curateFixture(t)
	out, errOut, c := hms(t, home, deadURL(), noModel, "", "curate", "-pending", "-p", "proj")
	if c != 0 {
		t.Fatalf("%d %s %s", c, out, errOut)
	}
	if !strings.HasPrefix(out, "[\n  {\n    \"project\": \"proj\"") {
		t.Fatalf("not indented JSON:\n%.200s", out)
	}
	var bs []struct {
		Project string
		Notes   []struct{ ID, Text string }
		Pages   []any
		Related []any
		Prompt  string
	}
	if err := json.Unmarshal([]byte(out), &bs); err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || len(bs[0].Notes) != 3 || bs[0].Notes[0].ID != ids[0] || bs[0].Notes[2].ID != ids[2] || bs[0].Prompt == "" || bs[0].Pages == nil || bs[0].Related == nil {
		t.Fatalf("%+v", bs)
	}
	all, _, _ := hms(t, home, deadURL(), noModel, "", "curate", "-pending")
	var both []struct{ Project string }
	json.Unmarshal([]byte(all), &both)
	if len(both) != 2 {
		t.Fatalf("all projects: %s", all)
	}
	one, _, _ := hms(t, home, deadURL(), noModel, "", "curate", "-pending", "-p", "proj", "-max", "2")
	var two []struct{ Notes []any }
	json.Unmarshal([]byte(one), &two)
	if len(two) != 1 || len(two[0].Notes) != 2 {
		t.Fatalf("-max 2: %s", one)
	}
	if out, _, c := hms(t, home, deadURL(), noModel, "", "curate", "-pending", "-p", "nothing"); c != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("no batch: %d %q", c, out)
	}
}

func submission(ids []string) string {
	body := "# Ops\n\n- The nightly job restarts the worker after 12 failed health checks and then pages the owner.\n- Backups run at 02:30 and keep 7 copies in the archive folder on the second disk.\n"
	lossy := "# Rules\n\nNo secrets in git.\n" // the note "Never commit secrets." is not there
	b, _ := json.Marshal(map[string]any{"project": "proj", "pages": []map[string]any{
		{"title": "Ops", "summary": "Operations notes", "tags": []string{"ops"}, "body": body, "notes": ids[:2]},
		{"title": "Rules", "summary": "House rules", "body": lossy, "notes": ids[2:3]},
	}})
	return string(b)
}

func TestCurateSubmitFromFileAndStdinOneCommit(t *testing.T) {
	home, repo, ids := curateFixture(t)
	file := filepath.Join(t.TempDir(), "answer.json")
	os.WriteFile(file, []byte(submission(ids)), 0o644)
	before := strings.TrimSpace(gitIn(t, repo, "rev-list", "--count", "HEAD"))
	out, errOut, c := hms(t, home, deadURL(), noModel, "", "curate", "-submit", file)
	if c != 0 {
		t.Fatalf("%d\n%s\n%s", c, out, errOut)
	}
	for _, want := range []string{"curate proj: 2 notes merged, 1 to Unsorted, 0 left, 0 unknown", "merged   Ops, 2 notes", "unsorted Rules, 1 notes", "note 33333333 is not in the page"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	if after := strings.TrimSpace(gitIn(t, repo, "rev-list", "--count", "HEAD")); after != fmt.Sprint(atoi(before)+1) {
		t.Fatalf("commits %s -> %s", before, after)
	}
	if subj := strings.TrimSpace(gitIn(t, repo, "log", "-1", "--format=%s")); subj != "curate proj: 2 notes into 1 pages (1 to Unsorted)" {
		t.Fatalf("subject %q", subj)
	}
	stat := gitIn(t, repo, "log", "-1", "--stat", "--format=")
	for _, want := range []string{"wiki/proj/Ops.md", "wiki/proj/Unsorted.md", "wiki/proj/Index.md", "notes/proj/11111111-aaaa.md", "21 deletions(-)"} {
		if !strings.Contains(stat, want) {
			t.Fatalf("stat lacks %q:\n%s", want, stat)
		}
	}
	idx, _ := os.ReadFile(filepath.Join(repo, "wiki/proj/Index.md"))
	if !strings.Contains(string(idx), "- [[Ops]] - Operations notes") || !strings.Contains(string(idx), "[[Unsorted]]") {
		t.Fatalf("Index.md:\n%s", idx)
	}
	if _, err := os.Stat(filepath.Join(repo, "notes/other/44444444-dddd.md")); err != nil {
		t.Fatal("another project's note was touched")
	}
	if gitIn(t, repo, "status", "--porcelain") != "" {
		t.Fatalf("uncommitted:\n%s", gitIn(t, repo, "status", "--porcelain"))
	}

	// from standard input, a bare model answer (fenced, with prose) and -p
	answer := "Here it is:\n```json\n" + `{"pages":[{"title":"Other things","summary":"s","body":"A note of another project that is waiting to be merged somewhere.","notes":["44444444-dddd"]}]}` + "\n```\n"
	out, errOut, c = hms(t, home, deadURL(), noModel, answer, "curate", "-submit", "-", "-p", "other")
	if c != 0 || !strings.Contains(out, "curate other: 1 notes merged") {
		t.Fatalf("stdin: %d\n%s\n%s", c, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(repo, "wiki/other/Other things.md")); err != nil {
		t.Fatal(err)
	}
}

func atoi(s string) int { var n int; fmt.Sscan(s, &n); return n }

func TestCurateSubmitErrorsExitOne(t *testing.T) {
	home, repo, ids := curateFixture(t)
	before := gitIn(t, repo, "rev-list", "--count", "HEAD")
	dup := `{"project":"proj","pages":[{"title":"A","body":"x","notes":["` + ids[0] + `"]},{"title":"B","body":"y","notes":["` + ids[0] + `"]}]}`
	for name, tc := range map[string]struct{ in, want string }{
		"two pages one note": {dup, "two pages"},
		"not json":           {"sorry", "no JSON"},
		"bare without -p":    {`{"pages":[]}`, "project"},
	} {
		out, e, c := hms(t, home, deadURL(), noModel, tc.in, "curate", "-submit", "-")
		if c != 1 || !strings.Contains(e, tc.want) || out != "" {
			t.Fatalf("%s: %d %q %q", name, c, out, e)
		}
	}
	if _, e, c := hms(t, home, deadURL(), noModel, "", "curate", "-submit", filepath.Join(t.TempDir(), "missing.json")); c != 1 || e == "" {
		t.Fatalf("missing file: %d %q", c, e)
	}
	if gitIn(t, repo, "rev-list", "--count", "HEAD") != before || gitIn(t, repo, "status", "--porcelain") != "" {
		t.Fatal("a failed submit changed the repository")
	}
}

func TestCurateApplyWithoutModel(t *testing.T) {
	home, repo, _ := curateFixture(t)
	out, errOut, c := hms(t, home, deadURL(), noModel, "", "curate", "-apply")
	if c != 1 || out != "" || errOut != "no model configured (HMS_LLM_URL, HMS_LLM_MODEL): use -pending and -submit from a harness job\n" {
		t.Fatalf("%d %q %q", c, out, errOut)
	}
	if _, e, c := hms(t, home, deadURL(), []string{"HMS_LLM_URL=http://localhost:1/v1", "HMS_LLM_MODEL="}, "", "curate", "-apply"); c != 1 || !strings.Contains(e, "no model configured") {
		t.Fatalf("url without model: %d %q", c, e)
	}
	if gitIn(t, repo, "status", "--porcelain") != "" {
		t.Fatal("changed")
	}
}

func TestCurateApplyWithAModelServer(t *testing.T) {
	home, repo, ids := curateFixture(t)
	var mode atomic.Value
	mode.Store("good")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == "error" {
			http.Error(w, "overloaded", 503)
			return
		}
		content := "I will not answer in JSON."
		if mode.Load() == "good" {
			b, _ := json.Marshal(map[string]any{"pages": []map[string]any{{"title": "Everything", "summary": "all of it", "body": "- The nightly job restarts the worker after 12 failed health checks and then pages the owner.\n- Backups run at 02:30 and keep 7 copies in the archive folder on the second disk.\n- Never commit secrets.\n", "notes": ids[:3]}}})
			content = "```json\n" + string(b) + "\n```"
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}})
	}))
	defer srv.Close()
	env := []string{"HMS_LLM_URL=" + srv.URL, "HMS_LLM_MODEL=m", "HMS_LLM_KEY="}

	mode.Store("error")
	out, e, c := hms(t, home, deadURL(), env, "", "curate", "-apply", "-p", "proj")
	if c != 1 || out != "" || !strings.Contains(e, "proj: notes left pending") || !strings.Contains(e, "503") {
		t.Fatalf("model error: %d %q %q", c, out, e)
	}
	mode.Store("bad")
	out, e, c = hms(t, home, deadURL(), env, "", "curate", "-apply", "-p", "proj")
	if c != 1 || !strings.Contains(e, "not understood") {
		t.Fatalf("bad answer: %d %q %q", c, out, e)
	}
	if gitIn(t, repo, "status", "--porcelain") != "" || len(mustGlob(t, repo, "notes/proj/*.md")) != 3 {
		t.Fatal("a failing model changed something")
	}

	mode.Store("good")
	out, e, c = hms(t, home, deadURL(), env, "", "curate", "-apply", "-p", "proj")
	if c != 0 || !strings.Contains(out, "curate proj: 3 notes merged, 0 to Unsorted") {
		t.Fatalf("good: %d %q %q", c, out, e)
	}
	if len(mustGlob(t, repo, "notes/proj/*.md")) != 0 || len(mustGlob(t, repo, "wiki/proj/Everything.md")) != 1 {
		t.Fatal("files")
	}
	if out, _, c := hms(t, home, deadURL(), env, "", "curate", "-apply", "-p", "proj"); c != 0 || out != "no notes waiting\n" {
		t.Fatalf("nothing left: %d %q", c, out)
	}
}

func mustGlob(t *testing.T, repo, pattern string) []string {
	m, err := filepath.Glob(filepath.Join(repo, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
