package curate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory/internal/md"
	"github.com/tomRumi/harness-memory/internal/store"
)

// writeNote writes a note file with a given created time, the way learn does, without going through learn.
func writeNote(t *testing.T, repo, project, id, text string, created time.Time) {
	t.Helper()
	fm := map[string]any{"kind": "learning", "created": created.UTC().Format(time.RFC3339Nano), "source": "learn"}
	p := filepath.Join(repo, "notes", project, id+".md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, md.Render(fm, text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPendingMaxOldestFirstEmptySkippedPerProject(t *testing.T) {
	st, repo := newStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 45; i++ { // the id order is the opposite of the age order
		writeNote(t, repo, proj, fmt.Sprintf("n%02d", i), fmt.Sprintf("Observation %d about the nightly job.", i), base.Add(time.Duration(44-i)*time.Minute))
	}
	writeNote(t, repo, proj, "empty", "   ", base.Add(-time.Hour))
	writeNote(t, repo, "other", "o1", "A note of the other project.", base)
	os.MkdirAll(filepath.Join(repo, "notes", "none"), 0o755)
	if _, err := st.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	bs, err := Pending(context.Background(), st, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].Project != "other" || bs[1].Project != proj {
		t.Fatalf("batches %+v", bs)
	}
	b := bs[1]
	if len(b.Notes) != 40 || b.Notes[0].ID != "n44" || b.Notes[39].ID != "n05" {
		t.Fatalf("%d notes, first %s last %s", len(b.Notes), b.Notes[0].ID, b.Notes[len(b.Notes)-1].ID)
	}
	if b.Notes[0].Text != "Observation 44 about the nightly job." {
		t.Fatal(b.Notes[0].Text)
	}
	small, _ := Pending(context.Background(), st, proj, 5)
	if len(small) != 1 || len(small[0].Notes) != 5 || small[0].Notes[4].ID != "n40" {
		t.Fatalf("%+v", small)
	}
	only, _ := Pending(context.Background(), st, "other", 0)
	if len(only) != 1 || only[0].Notes[0].ID != "o1" {
		t.Fatal("project filter")
	}
	none, _ := Pending(context.Background(), st, "none", 0)
	if len(none) != 0 {
		t.Fatal("a project without notes has no batch")
	}
	// the JSON has the documented shape
	raw, _ := json.Marshal(b)
	var m map[string]any
	json.Unmarshal(raw, &m)
	for _, k := range []string{"project", "notes", "pages", "related", "prompt"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("batch JSON lacks %q: %s", k, raw[:200])
		}
	}
	if !strings.Contains(b.Prompt, `"pages"`) || !strings.Contains(b.Prompt, "base_hash") || !strings.Contains(b.Prompt, "[[Page Title]]") {
		t.Fatal(b.Prompt)
	}
}

func TestPendingPagesListExcludesIndexAndIsSorted(t *testing.T) {
	st, repo := newStore(t)
	putFile(t, st, "wiki/proj/Zebra.md", "---\nsummary: Stripes\n---\nBody.\n")
	putFile(t, st, "wiki/proj/alpha.md", "The first sentence is the summary. More.\n")
	putFile(t, st, "wiki/proj/Index.md", "---\ngenerated: true\n---\n# Index\n")
	putFile(t, st, "wiki/proj/sub/Deep.md", "Deep page.\n")
	putFile(t, st, "wiki/other/Foreign.md", "Another project.\n")
	writeNote(t, repo, proj, "a1", "Something to place.", time.Now())
	bs, err := Pending(context.Background(), st, proj, 0)
	if err != nil || len(bs) != 1 {
		t.Fatalf("%v %v", err, bs)
	}
	var titles []string
	for _, p := range bs[0].Pages {
		titles = append(titles, p.Title)
	}
	if strings.Join(titles, ",") != "alpha,Deep,Zebra" {
		t.Fatalf("titles %v", titles)
	}
	if bs[0].Pages[0].Summary != "The first sentence is the summary." || bs[0].Pages[2].Summary != "Stripes" || bs[0].Pages[2].Path != "wiki/proj/Zebra.md" {
		t.Fatalf("%+v", bs[0].Pages)
	}
}

func TestPendingRelatedPagesBySimilarityBigPageExcludedHashPresent(t *testing.T) {
	st, repo := newStore(t)
	db := "---\nsummary: Database care\n---\n# Databases\n\nPostgres autovacuum removes dead rows; run vacuum when table bloat grows.\n"
	putFile(t, st, "wiki/proj/Databases.md", db)
	putFile(t, st, "wiki/proj/Colors.md", "Red green and blue are primary colours of light.\n")
	putFile(t, st, "wiki/proj/Ports.md", "The daemon listens on a loopback port.\n")
	huge := "Postgres vacuum bloat autovacuum notes.\n" + strings.Repeat("Postgres vacuum bloat autovacuum tuning and more words here.\n", 800)
	if len(huge) <= MaxRelatedBody {
		t.Fatal("test page is not big enough")
	}
	putFile(t, st, "wiki/proj/Huge.md", huge)
	writeNote(t, repo, proj, "p1", "Run vacuum on the big postgres table when bloat grows past a third.", time.Now())
	writeNote(t, repo, proj, "p2", "Autovacuum can fall behind on postgres tables with heavy updates.", time.Now())
	if _, err := st.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	bs, err := Pending(context.Background(), st, proj, 0)
	if err != nil {
		t.Fatal(err)
	}
	rel := bs[0].Related
	if len(rel) == 0 || rel[0].Title != "Databases" {
		t.Fatalf("related %+v", rel)
	}
	for _, r := range rel {
		if r.Title == "Huge" {
			t.Fatal("a page over 40,000 bytes must not be handed over")
		}
		if r.Title == "Colors" && rel[0].Title != "Databases" {
			t.Fatal("an unrelated page ranks first")
		}
	}
	if rel[0].Hash != Hash([]byte(db)) || len(rel[0].Hash) != 64 {
		t.Fatalf("hash %s", rel[0].Hash)
	}
	if !strings.Contains(rel[0].Body, "autovacuum removes dead rows") || strings.Contains(rel[0].Body, "summary:") {
		t.Fatalf("body %q", rel[0].Body)
	}
	// the batch lists the huge page in pages, with its title, so a link to it is still allowed
	found := false
	for _, p := range bs[0].Pages {
		found = found || p.Title == "Huge"
	}
	if !found {
		t.Fatal("Huge missing from pages")
	}
}

func TestPendingRelatedAtMostFiveAndKeywordsOnlyWhenEmbedderIsDown(t *testing.T) {
	st, repo := newStore(t)
	for i := 0; i < 8; i++ {
		putFile(t, st, fmt.Sprintf("wiki/proj/Vacuum page %d.md", i), fmt.Sprintf("Page %d explains the vacuum job.\n", i))
	}
	writeNote(t, repo, proj, "v1", "The vacuum job runs nightly.", time.Now())
	st.Emb.(*bag).fail.Store(true)
	bs, err := Pending(context.Background(), st, proj, 0)
	if err != nil || len(bs[0].Related) != MaxRelated {
		t.Fatalf("%v related %d", err, len(bs[0].Related))
	}
}

func TestPendingHashDetectsAHandEditBeforeSubmit(t *testing.T) {
	st, repo := newStore(t)
	putFile(t, st, "wiki/proj/Databases.md", "# Databases\n\nPostgres vacuum notes.\n")
	notes := learnN(t, st, proj, 2, "Postgres vacuum observation %d: run it when the table has %d percent bloat.")
	bs, _ := Pending(context.Background(), st, proj, 0)
	if len(bs[0].Related) != 1 {
		t.Fatalf("related %+v", bs[0].Related)
	}
	r := bs[0].Related[0]
	putFile(t, st, "wiki/proj/Databases.md", r.Body+"Edited by a person meanwhile.\n")
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{{Title: r.Title, Body: r.Body + bullets(notes), Notes: ids(notes), BaseHash: r.Hash}}})
	if err != nil || rep.Pages[0].Status != Stale || len(rep.Left) != 2 {
		t.Fatalf("%v %+v", err, rep)
	}
	if !strings.Contains(readFile(t, st, "wiki/proj/Databases.md"), "Edited by a person") {
		t.Fatal("the hand edit is gone")
	}
	_ = repo
}

func TestWaitingNotes(t *testing.T) {
	_, repo := newStore(t)
	writeNote(t, repo, "a", "a1", "One.", time.Now().Add(-3*time.Hour))
	writeNote(t, repo, "a", "a2", "Two.", time.Now().Add(-time.Hour))
	writeNote(t, repo, "b", "b1", "Three.", time.Now().Add(-72*time.Hour))
	ws, err := WaitingNotes(repo)
	if err != nil || len(ws) != 2 || ws[0].Project != "a" || ws[0].Notes != 2 || ws[1].Notes != 1 {
		t.Fatalf("%v %+v", err, ws)
	}
	if Age(ws[0].Oldest) != "3 h" || Age(ws[1].Oldest) != "3 d" || Age(5*time.Minute) != "5 min" {
		t.Fatalf("%s %s", Age(ws[0].Oldest), Age(ws[1].Oldest))
	}
}

func TestParseAnswer(t *testing.T) {
	good := `{"pages":[{"title":"A","summary":"s","tags":["x"],"body":"b {not json}","notes":["n1"],"base_hash":"h"}]}`
	for name, in := range map[string]string{
		"plain":        good,
		"fenced":       "```json\n" + good + "\n```",
		"fence no tag": "```\n" + good + "\n```\n",
		"prose around": "Here is the answer {as requested}:\n" + good + "\nHope this helps! {done}",
		"crlf":         strings.ReplaceAll("```json\n"+good+"\n```", "\n", "\r\n"),
	} {
		ps, err := ParseAnswer(in)
		if err != nil || len(ps) != 1 || ps[0].Title != "A" || ps[0].Body != "b {not json}" || ps[0].Notes[0] != "n1" || ps[0].BaseHash != "h" || ps[0].Tags[0] != "x" {
			t.Fatalf("%s: %v %+v", name, err, ps)
		}
	}
	for name, in := range map[string]string{
		"broken":     `{"pages":[{"title":"A","body":`,
		"no json":    "I cannot do that.",
		"empty":      "",
		"no pages":   `{"title":"A"}`,
		"wrong type": `{"pages":"x"}`,
	} {
		if ps, err := ParseAnswer(in); err == nil {
			t.Fatalf("%s: accepted %+v", name, ps)
		}
	}
	if ps, err := ParseAnswer(`{"pages":[]}`); err != nil || len(ps) != 0 {
		t.Fatalf("empty pages: %v %v", err, ps)
	}
}

func TestParseSubmissionAcceptsBothShapes(t *testing.T) {
	full := `{"project":"p","pages":[{"title":"A","body":"b","notes":[]}]}`
	bare := `{"pages":[{"title":"A","body":"b","notes":[]}]}`
	if s, err := ParseSubmission(full, ""); err != nil || s.Project != "p" || len(s.Pages) != 1 {
		t.Fatalf("%v %+v", err, s)
	}
	if s, err := ParseSubmission(bare, "q"); err != nil || s.Project != "q" {
		t.Fatalf("%v %+v", err, s)
	}
	if _, err := ParseSubmission(bare, ""); err == nil {
		t.Fatal("a bare answer needs -p")
	}
	if _, err := ParseSubmission(full, "q"); err == nil {
		t.Fatal("a project mismatch must fail")
	}
	if s, err := ParseSubmission(full, "p"); err != nil || s.Project != "p" {
		t.Fatalf("%v %+v", err, s)
	}
}

// chatServer answers like a chat-completions server. The project in the request decides the behaviour: "good"
// answers well (in a fence, with prose), "bad" answers with text that is not JSON, "err" answers 500.
func chatServer(t *testing.T) (*httptest.Server, *int) {
	calls := new(int)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct{ Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/chat/completions" || req.Model != "m" || req.MaxTokens != MaxTokens || len(req.Messages) != 1 {
			http.Error(w, "unexpected request", 400)
			return
		}
		msg := req.Messages[0].Content
		_, input, _ := strings.Cut(msg, "\nInput:\n")
		var b Batch
		if err := json.Unmarshal([]byte(input), &b); err != nil {
			http.Error(w, "input: "+err.Error(), 400)
			return
		}
		var content string
		switch b.Project {
		case "good":
			pg := Page{Title: "Workers", Summary: "How workers restart", Tags: []string{"ops"}, Body: "# Workers\n\n" + bullets(b.Notes), Notes: ids(b.Notes)}
			raw, _ := json.Marshal(map[string]any{"pages": []Page{pg}})
			content = "Sure, here you go:\n```json\n" + string(raw) + "\n```\nLet me know."
		case "bad":
			content = "I am sorry, I cannot merge these notes."
		default:
			http.Error(w, "boom", 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}})
	}))
	t.Cleanup(s.Close)
	return s, calls
}

func TestApplyEndToEndGoodBadAndErrorModel(t *testing.T) {
	st, repo := newStore(t)
	goodNotes := learnN(t, st, "good", 3, tmplWorker)
	learnN(t, st, "bad", 2, tmplWorker)
	learnN(t, st, "err", 2, tmplWorker)
	commits(t, st, repo)
	srv, calls := chatServer(t)
	llm := &LLM{URL: srv.URL, Model: "m", HTTP: srv.Client()}
	res, err := Apply(context.Background(), st, llm, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || *calls != 3 {
		t.Fatalf("%d results, %d calls", len(res), *calls)
	}
	by := map[string]Applied{}
	for _, a := range res {
		by[a.Project] = a
	}
	if g := by["good"]; g.Err != nil || g.Report == nil || g.Report.Merged != 3 {
		t.Fatalf("good: %+v %v", g.Report, g.Err)
	}
	if b := by["bad"]; b.Err == nil || !strings.Contains(b.Err.Error(), "not understood") || b.Report != nil {
		t.Fatalf("bad: %v", b.Err)
	}
	if e := by["err"]; e.Err == nil || !strings.Contains(e.Err.Error(), "500") {
		t.Fatalf("err: %v", e.Err)
	}
	// good: merged; bad and err: every note still pending, no Unsorted page
	if noteCount(t, st, "good") != 0 || noteCount(t, st, "bad") != 2 || noteCount(t, st, "err") != 2 {
		t.Fatal("note counts")
	}
	if exists(st, "wiki/bad/Unsorted.md") || exists(st, "wiki/err/Unsorted.md") || exists(st, "wiki/bad/Index.md") {
		t.Fatal("a model failure must not write anything")
	}
	if !strings.Contains(readFile(t, st, "wiki/good/Workers.md"), goodNotes[0].Text) {
		t.Fatal("page content")
	}
}

func TestModelMessageHasInstructionThenInput(t *testing.T) {
	b := Batch{Project: "p", Notes: []Note{{ID: "i1", Text: "t1"}}, Pages: []PageRef{}, Related: []Related{}}
	b.Prompt = BuildPrompt(b)
	m := ModelMessage(b)
	head, input, ok := strings.Cut(m, "\nInput:\n")
	if !ok || head != b.Prompt || strings.Contains(input, "prompt") || !strings.Contains(input, `"i1"`) {
		t.Fatal(m)
	}
}

// An unverified note is not handed out, and a submission that names it neither merges nor deletes it.
func TestUnverifiedNoteIsNeitherPendingNorMerged(t *testing.T) {
	st, _ := newStore(t)
	ctx := context.Background()
	notes := learnN(t, st, proj, 3, tmplWorker)
	staged, err := st.LearnAs(ctx, "A staged observation about worker restarts that nobody has checked.", proj, "", "", &store.Writer{Author: "scout", Staged: true})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := Pending(ctx, st, proj, 0)
	if err != nil || len(bs) != 1 || len(bs[0].Notes) != 3 {
		t.Fatalf("%v %+v", err, bs)
	}
	for _, n := range bs[0].Notes {
		if n.ID == staged {
			t.Fatal("an unverified note was handed out")
		}
	}
	rep, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{
		{Title: "Workers", Summary: "w", Body: "# Workers\n\n" + bullets(notes[:2]), Notes: append(ids(notes[:2]), staged)},
		{Title: "Others", Summary: "o", Body: "# Others\n\n" + bullets(notes[2:]), Notes: ids(notes[2:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages[0].Status != Stale || rep.Pages[1].Status != Merged {
		t.Fatalf("%+v", rep.Pages)
	}
	if !exists(st, "notes/"+proj+"/"+staged+".md") || !strings.Contains(readFile(t, st, "notes/"+proj+"/"+staged+".md"), "status: unverified") {
		t.Fatal("the unverified note was touched")
	}
	if exists(st, "wiki/"+proj+"/Workers.md") {
		t.Fatal("a page naming an unverified note was written")
	}
	if got := noteCount(t, st, proj); got != 2 {
		t.Fatalf("%d notes left, want the 2 of the stale page", got)
	}
}
