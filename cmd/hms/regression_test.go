package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tomRumi/harness-memory-service/internal/curate"
)

// Item 5: -p must be lower-case; nothing named PROJ is ever made.
func TestCurateRefusesAnUpperCaseProject(t *testing.T) {
	home, repo, _ := curateFixture(t)
	for _, args := range [][]string{{"curate", "-p", "PROJ"}, {"curate", "-pending", "-p", "PROJ"}, {"curate", "-submit", "-", "-p", "PROJ"}, {"wiki", "index", "-p", "PROJ"}} {
		_, e, c := hms(t, home, deadURL(), noModel, `{"pages":[{"title":"A","body":"x","notes":[]}]}`, args...)
		if c == 0 || !strings.Contains(e, "project names are lower-case: proj") {
			t.Errorf("%v: %d %q", args, c, e)
		}
	}
	for _, d := range []string{"wiki", "notes"} { // exact names: a case-insensitive file system answers Stat("PROJ") for proj
		ents, _ := os.ReadDir(filepath.Join(repo, d))
		for _, e := range ents {
			if e.Name() == "PROJ" {
				t.Errorf("%s/PROJ exists", d)
			}
		}
	}
}

func rate(t *testing.T, home, id, kind string, sessions int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < sessions; i++ {
		if _, err := db.Exec(`INSERT INTO events(at,kind,node_id,project,detail) VALUES('2026-11-01T00:00:00Z',?,?,?,?)`, kind, "notes/proj/"+id, "proj", fmt.Sprintf("s%d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

// Item 8: ratings only list; `hms curate` prints the candidates, a submit deletes nothing.
func TestCurateListsRatedNotesAndNeverDeletesThem(t *testing.T) {
	home, repo, ids := curateFixture(t)
	// events.db is created by the store; make it by running a command that opens it
	hms(t, home, deadURL(), noModel, "", "curate")
	rate(t, home, ids[2], "unhelpful", 5)
	out, _, c := hms(t, home, deadURL(), noModel, "", "curate")
	want := "rated unhelpful by 5 sessions, none helpful: " + ids[2]
	if c != 0 || !strings.Contains(out, want) {
		t.Fatalf("%d\n%s", c, out)
	}
	rate(t, home, ids[2], "helpful", 1)
	if out, _, _ := hms(t, home, deadURL(), noModel, "", "curate"); strings.Contains(out, "rated unhelpful") {
		t.Fatalf("a helpful rating keeps it off the list:\n%s", out)
	}
	rate(t, home, ids[1], "unhelpful", 4)
	if out, _, _ := hms(t, home, deadURL(), noModel, "", "curate"); strings.Contains(out, "rated unhelpful") {
		t.Fatalf("four sessions are not enough:\n%s", out)
	}
	rate(t, home, ids[0], "unhelpful", 5)
	sub, _ := json.Marshal(map[string]any{"project": "proj", "pages": []map[string]any{{"title": "Backups", "summary": "s", "body": "# Backups\n\n- Backups run at 02:30 and keep 7 copies in the archive folder on the second disk.\n", "notes": []string{ids[1]}}}})
	if out, e, c := hms(t, home, deadURL(), noModel, string(sub), "curate", "-submit", "-"); c != 0 || strings.Contains(out, "retired") {
		t.Fatalf("%d %s %s", c, out, e)
	}
	for _, id := range []string{ids[0], ids[2]} {
		if _, err := os.Stat(filepath.Join(repo, "notes", "proj", id+".md")); err != nil {
			t.Errorf("rated note %s was deleted by the merge", id)
		}
	}
}

// Item 14: the listing counts the notes whose name cannot be an id.
func TestCurateCountsUnusableNoteNames(t *testing.T) {
	home, repo, _ := curateFixture(t)
	os.WriteFile(filepath.Join(repo, "notes", "proj", "bad\x07name.md"), []byte("---\nkind: learning\n---\n\nSome text of a note with a bad name.\n"), 0o644)
	out, _, c := hms(t, home, deadURL(), noModel, "", "curate")
	if c != 0 || !strings.Contains(out, "1 note files have a name that cannot be a note id") || !strings.Contains(out, "proj: 3 notes waiting") {
		t.Fatalf("%d\n%s", c, out)
	}
}

// Item 13: `hms wiki index` says the hand-written Index.md is left alone.
func TestWikiIndexLeavesAHandWrittenIndex(t *testing.T) {
	home, repo, _ := curateFixture(t)
	os.MkdirAll(filepath.Join(repo, "wiki", "proj"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "proj", "Index.md"), []byte("# Mine\n\nHand-written.\n"), 0o644)
	out, _, c := hms(t, home, deadURL(), nil, "", "wiki", "index", "-p", "proj")
	if c != 0 || !strings.Contains(out, "Index.md is hand-written; left alone") {
		t.Fatalf("%d %q", c, out)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "wiki", "proj", "Index.md")); !strings.Contains(string(b), "Hand-written.") {
		t.Fatal("overwritten")
	}
}

// Item 4: two `hms curate -submit` processes at once never lose a note: the second ends stale (or merged).
func TestTwoSubmitProcessesLoseNoNote(t *testing.T) {
	for round := 0; round < 4; round++ {
		home, repo, ids := curateFixture(t)
		old := "# Ports\n\nThe daemon uses a port.\n"
		os.MkdirAll(filepath.Join(repo, "wiki", "proj"), 0o755)
		os.WriteFile(filepath.Join(repo, "wiki", "proj", "Ports.md"), []byte(old), 0o644)
		texts := map[string]string{
			ids[0]: "The nightly job restarts the worker after 12 failed health checks and then pages the owner.",
			ids[1]: "Backups run at 02:30 and keep 7 copies in the archive folder on the second disk.",
		}
		var wg sync.WaitGroup
		outs := make([]string, 2)
		for i, id := range ids[:2] {
			sub, _ := json.Marshal(map[string]any{"project": "proj", "pages": []map[string]any{{"title": "Ports", "body": old + "\n- " + texts[id] + "\n", "notes": []string{id}, "base_hash": curate.Hash([]byte(old))}}})
			wg.Add(1)
			go func(i int, sub string) {
				defer wg.Done()
				o, e, _ := hms(t, home, deadURL(), noModel, sub, "curate", "-submit", "-")
				outs[i] = o + e
			}(i, string(sub))
		}
		wg.Wait()
		page, _ := os.ReadFile(filepath.Join(repo, "wiki", "proj", "Ports.md"))
		stale := 0
		for _, id := range ids[:2] {
			_, err := os.Stat(filepath.Join(repo, "notes", "proj", id+".md"))
			inPage := strings.Contains(string(page), texts[id])
			if err != nil && !inPage {
				t.Fatalf("round %d: note %s is deleted and in no page\n%s\n%s", round, id, outs[0], outs[1])
			}
			if err == nil {
				stale++
			}
		}
		if stale != 1 {
			t.Fatalf("round %d: %d notes still waiting, want exactly one (the second submit is stale)\n%s\n%s", round, stale, outs[0], outs[1])
		}
	}
}

// Items 1, 8 and 19: what the documents say.
func TestDocumentsSayWhatTheProgramDoes(t *testing.T) {
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	recipe := read("integrations/curation-by-a-harness.md")
	if strings.Contains(recipe, "reset --hard") || strings.Contains(recipe, "throws its work away") {
		t.Error("the recipe still tells the job to reset --hard")
	}
	for _, want := range []string{"git pull --rebase", "git rebase --abort"} {
		if !strings.Contains(recipe, want) {
			t.Errorf("the recipe lacks %q", want)
		}
	}
	design := read("docs/design.md")
	if strings.Contains(design, "throws its work away") || strings.Contains(design, "reset --hard") {
		t.Error("design.md still says the job throws its work away")
	}
	if !strings.Contains(design, "git rebase --abort") {
		t.Error("design.md 3.3 lacks the safe rule")
	}
	for _, f := range []string{"README.md", "docs/reference.md", "docs/design.md", "integrations/any-mcp-client.md"} {
		if s := read(f); strings.Contains(s, "is deleted at the next merge") || strings.Contains(s, "poor writer cannot lose") {
			t.Errorf("%s: outdated sentence", f)
		}
	}
	if s := read("integrations/hermes/README.md"); strings.Contains(s, "(step 0a)") {
		t.Error("hermes README: step 0a")
	}
	if s := read("README.md"); !strings.Contains(s, "more than 160 automated tests") || !strings.Contains(s, "its notes go to Unsorted") {
		t.Error("README: test count or the Unsorted sentence")
	}
	if s := read("docs/reference.md"); !strings.Contains(s, "golang.org/x/text") || strings.Contains(s, "retired by the merge") {
		t.Error("reference: dependency table or retire text")
	}
}
