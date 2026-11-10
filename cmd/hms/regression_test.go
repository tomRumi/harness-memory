package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tomRumi/harness-memory/internal/curate"
)

// Item 5: -p PROJ means proj; nothing named PROJ is ever made.
func TestCurateLowerCasesAnUpperCaseProject(t *testing.T) {
	home, repo, _ := curateFixture(t)
	for _, args := range [][]string{{"curate", "-p", "PROJ"}, {"curate", "-pending", "-p", "PROJ"}, {"curate", "-submit", "-", "-p", "PROJ"}, {"wiki", "index", "-p", "PROJ"}} {
		_, e, c := hms(t, home, deadURL(), noModel, `{"pages":[{"title":"A","body":"x","notes":[]}]}`, args...)
		if strings.Contains(e, "invalid project") || strings.Contains(e, "lower-case") {
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
	if s := read("README.md"); !strings.Contains(s, "automated tests") || !strings.Contains(s, "goes to Unsorted") {
		t.Error("README: test count or the Unsorted sentence")
	}
	if s := read("docs/reference.md"); !strings.Contains(s, "golang.org/x/text") || strings.Contains(s, "retired by the merge") {
		t.Error("reference: dependency table or retire text")
	}
}

// The next writing command commits what a killed run left behind (an accented page and a note deletion).
func TestNextWritingCommandCommitsLeftovers(t *testing.T) {
	home, repo, ids := curateFixture(t)
	os.Remove(filepath.Join(repo, "notes", "other", ids[3]+".md"))
	os.MkdirAll(filepath.Join(repo, "wiki", "other"), 0o755)
	os.WriteFile(filepath.Join(repo, "wiki", "other", "Étude.md"), []byte("# Étude\n\nA note of another project that is waiting to be merged somewhere.\n"), 0o644)
	if _, e, c := hms(t, home, deadURL(), []string{"HMS_VECTORS=0"}, "", "wiki", "index", "-p", "proj"); c != 0 {
		t.Fatalf("wiki index: %s", e)
	}
	if s := strings.TrimSpace(gitIn(t, repo, "status", "--porcelain", "-uall")); s != "" {
		t.Errorf("left loose by the next command: %q", s)
	}
	if !strings.Contains(gitIn(t, repo, "-c", "core.quotepath=false", "log", "--name-status", "--format=", "-3"), "Étude.md") {
		t.Error("the accented page is in no commit")
	}
}

// Item 3: the kill script (a real SIGKILL during the slow copy check).
func TestCrashScript(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed by the script")
	}
	out, err := exec.Command("sh", filepath.Join("..", "..", "scripts", "test", "crash.sh")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("crash.sh: %v\n%s", err, out)
	}
}

// Items 6 and 10: the recipe says what to do with an unclean repository, and the documents claim only what is true.
func TestRecipeAndDocumentsAfterTheSecondReview(t *testing.T) {
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	recipe, design := read("integrations/curation-by-a-harness.md"), read("docs/design.md")
	for name, text := range map[string]string{"recipe": recipe, "design.md": design} {
		for _, want := range []string{"git status --porcelain", "untracked", "rebase", "detached", "hms doctor", "FAIL", "git rebase --abort"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	// the numbered step 1 and the prompt both carry the rule, and the Conflicts paragraph follows the prompt
	steps, prompt, _ := strings.Cut(recipe, "## The job prompt")
	if !strings.Contains(steps, "1. Check the repository first") || !strings.Contains(prompt, "git status --porcelain") || !strings.Contains(prompt, "hms doctor") {
		t.Error("step 1 and the prompt are not in sync")
	}
	if !strings.Contains(recipe, "## Conflicts") || strings.Index(recipe, "## Conflicts") < strings.Index(recipe, "## The job prompt") {
		t.Error("no Conflicts paragraph after the prompt")
	}
	readme, ref, hermes := read("README.md"), read("docs/reference.md"), read("integrations/hermes/README.md")
	for name, text := range map[string]string{"README": readme, "reference": ref, "design": design, "recipe": recipe} {
		for _, bad := range []string{"checks that nothing was lost", "every note's words and numbers arrived", "so nothing is lost", "the index and the settings", "One committer makes the commits", "predecessor", "hermes-memory-rag"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s still says %q", name, bad)
			}
		}
	}
	if strings.Contains(hermes, "unchanged") {
		t.Error("hermes README still says the prompt ran unchanged")
	}
	for _, want := range []string{"8-word run", "core.hooksPath scripts/hooks", "goes to Unsorted"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	for _, want := range []string{"core.hooksPath scripts/hooks", "One committer per process", "200 bytes", "`Unsorted`", "case-insensitively and Unicode-normalised"} {
		if !strings.Contains(ref, want) {
			t.Errorf("reference lacks %q", want)
		}
	}
}

// Third review: a commit that fails is said so (stderr, the report) and the exit status is 1; the files stay saved.
func TestFailedCommitIsReportedByEveryWritingCommand(t *testing.T) {
	home, repo, ids := curateFixture(t)
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte("#!/bin/sh\necho refused by the test hook\nexit 1\n"), 0o755)
	file := filepath.Join(t.TempDir(), "answer.json")
	os.WriteFile(file, []byte(submission(ids)), 0o644)
	out, errOut, c := hms(t, home, deadURL(), noModel, "", "curate", "-submit", file)
	if c != 1 || !strings.Contains(errOut, "committed: NO") || !strings.Contains(errOut, "run the command again") || !strings.Contains(out, "committed: NO") {
		t.Fatalf("submit: exit %d\nstdout: %s\nstderr: %s", c, out, errOut)
	}
	if s := strings.TrimSpace(gitIn(t, repo, "diff", "--cached", "--name-only")); s != "" {
		t.Errorf("left staged: %q", s)
	}
	if _, err := os.Stat(filepath.Join(repo, "wiki", "proj", "Ops.md")); err != nil {
		t.Errorf("the files are not saved: %v", err)
	}
	// wiki index: the same rule
	_, errOut, c = hms(t, home, deadURL(), []string{"HMS_VECTORS=0"}, "", "wiki", "index", "-p", "other")
	if c != 1 || !strings.Contains(errOut, "committed: NO") {
		t.Errorf("wiki index: exit %d: %s", c, errOut)
	}
	// the hook is fixed: the next writing command commits everything that was left
	os.Remove(hook)
	if _, errOut, c = hms(t, home, deadURL(), []string{"HMS_VECTORS=0"}, "", "wiki", "index", "-p", "other"); c != 0 {
		t.Fatalf("after the fix: exit %d: %s", c, errOut)
	}
	if s := strings.TrimSpace(gitIn(t, repo, "status", "--porcelain", "-uall")); s != "" {
		t.Errorf("left loose after the fix: %q", s)
	}
}

// Third review, recipe: the text names the git outputs it relies on, and git really prints them (scripts/test/recipe-git.sh).
func TestRecipeStopsOnADivergedOrConflictedPull(t *testing.T) {
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	recipe, design := read("integrations/curation-by-a-harness.md"), read("docs/design.md")
	steps, rest, _ := strings.Cut(recipe, "## The job prompt")
	prompt, _, _ := strings.Cut(rest, "## Conflicts")
	step1 := steps[strings.Index(steps, "1. Check the repository first"):strings.Index(steps, "\n2. ")]
	promptStep1 := prompt[strings.Index(prompt, "1. cd to"):strings.Index(prompt, "\n2. Run")]
	for name, text := range map[string]string{"recipe step 1": step1, "prompt step 1": promptStep1} {
		for _, want := range []string{"git pull --rebase", "git status", "have diverged", "Unmerged paths", "git rebase --abort", "Never reset"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if i, j := strings.Index(text, "git pull --rebase"), strings.LastIndex(text, "have diverged"); i < 0 || j < i {
			t.Errorf("%s: the status check must come after the pull", name)
		}
	}
	for _, want := range []string{"have diverged", "git rebase --abort", "ahead and behind"} {
		if !strings.Contains(design, want) {
			t.Errorf("design lacks %q", want)
		}
	}
	for name, text := range map[string]string{"design": design, "recipe": recipe, "reference": read("docs/reference.md")} {
		if strings.Contains(text, "committed by the next `hms` command that writes.") {
			t.Errorf("%s still claims every leftover is committed", name)
		}
		if strings.Contains(text, "as Obsidian and macOS do") || strings.Contains(text, "as the file system does") {
			t.Errorf("%s still overclaims the title comparison", name)
		}
	}
	for name, text := range map[string]string{"design": design, "recipe": recipe, "reference": read("docs/reference.md")} {
		if strings.Contains(text, "shows a blocked repository") {
			t.Errorf("%s still says doctor shows a blocked repository after a failed commit", name)
		}
		if !strings.Contains(text, "project folders with capital letters") {
			t.Errorf("%s does not say what doctor shows", name)
		}
	}
	if ref := read("docs/reference.md"); !strings.Contains(ref, "except by the copy warning of a merge") || strings.Contains(ref, "are refused as well") || strings.Contains(design, "treats as one are refused") {
		t.Error("the embedder or the name-collision overclaim is back")
	}
	if strings.Contains(recipe, "until the repository is clean") {
		t.Error("the Conflicts paragraph still overclaims")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is needed by the script")
	}
	out, err := exec.Command("sh", filepath.Join("..", "..", "scripts", "test", "recipe-git.sh")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("recipe-git.sh: %v\n%s", err, out)
	}
}
