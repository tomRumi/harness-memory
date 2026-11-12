package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory/internal/access"
	"github.com/tomRumi/harness-memory/internal/index"
)

// checkedSetup is tokenSetup with five writers in project a: "st" is staged (named scout), "rv" is staged (named reviewer), "wn" is named alice, "ta"
// has no name, "ver" may verify.
func checkedSetup(t *testing.T) *tenancy {
	t.Helper()
	x := tokenSetup(t)
	tokenFile(t, x.dir, "a", map[string]string{
		"st":  `"write":true,"learn_status":"unverified","name":"scout"`,
		"rv":  `"write":true,"learn_status":"unverified","name":"reviewer"`,
		"wn":  `"write":true,"name":"alice"`,
		"ta":  `"write":true`,
		"ver": `"write":true,"verify":true,"name":"checker"`,
	})
	return x
}

func noteFile(t *testing.T, x *tenancy, id string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(x.repo, "notes", "a", id+".md"))
	return string(b), err == nil
}

func TestStagedLearnLandsUnverifiedWithAuthor(t *testing.T) {
	x := checkedSetup(t)
	id, isErr := call(t, x.client(t, "st"), "learn", map[string]any{"text": "Quartz clocks drift by the temperature."})
	if isErr {
		t.Fatal(id)
	}
	raw, _ := noteFile(t, x, id)
	if !strings.Contains(raw, "status: unverified\n") || !strings.Contains(raw, "author: scout\n") {
		t.Fatalf("file:\n%s", raw)
	}
	out, _ := call(t, x.client(t, "wn"), "recall", map[string]any{"query": "quartz clocks", "layer": "memory"})
	if !strings.Contains(out, " #"+id[:8]+" unverified, by scout]") {
		t.Fatalf("recall must mark the hit:\n%s", out)
	}
	block, _ := x.srv.ContextBlock(context.Background(), "quartz clocks", "a", 0)
	if !strings.Contains(block, "unverified, by scout") {
		t.Fatalf("the hook block lost the mark:\n%s", block)
	}
	// the same text again: nothing changes, and the answer says the note is unverified
	again, _ := call(t, x.client(t, "wn"), "learn", map[string]any{"text": "Quartz clocks drift by the temperature."})
	if !strings.HasPrefix(again, id) || !strings.Contains(again, "unverified") {
		t.Fatalf("idempotent answer: %q", again)
	}
	if after, _ := noteFile(t, x, id); after != raw {
		t.Fatal("a repeated learn changed the file")
	}
}

func TestTokenLearnIsActiveWithAuthorAndOpenModeIsUnchanged(t *testing.T) {
	x := checkedSetup(t)
	id, _ := call(t, x.client(t, "wn"), "learn", map[string]any{"text": "Alice found the lamp."})
	raw, _ := noteFile(t, x, id)
	if strings.Contains(raw, "status") || !strings.Contains(raw, "author: alice\n") {
		t.Fatalf("named writer:\n%s", raw)
	}
	id, _ = call(t, x.client(t, "ta"), "learn", map[string]any{"text": "An unnamed writer found the lamp."})
	raw, _ = noteFile(t, x, id)
	if strings.Contains(raw, "status") || !strings.Contains(raw, "author: "+access.Digest("ta")[:8]+"\n") {
		t.Fatalf("unnamed writer:\n%s", raw)
	}
	id, _ = x.srv.St.Learn(context.Background(), "An open mode note.", "a", "", "")
	if raw, _ = noteFile(t, x, id); strings.Contains(raw, "status") || strings.Contains(raw, "author") {
		t.Fatalf("open mode:\n%s", raw)
	}
}

func TestStagedTokenTouchesOnlyUnverifiedNotes(t *testing.T) {
	x := checkedSetup(t)
	ctx := context.Background()
	st, wn := x.client(t, "st"), x.client(t, "wn")
	active, _ := call(t, wn, "learn", map[string]any{"text": "An active note that must stay."})
	fact, _ := x.srv.St.AddFact(ctx, "An active fact that must stay.", "a", "", 0)
	// supersede an active note: the new note is stored, the old one stays, the answer says why
	out, isErr := call(t, st, "learn", map[string]any{"text": "A staged attempt to replace it.", "supersedes": active[:8]})
	if isErr || !strings.Contains(out, "nothing was marked as replaced") {
		t.Fatalf("%v %q", isErr, out)
	}
	if _, ok := noteFile(t, x, active); !ok {
		t.Fatal("a staged token replaced an active note")
	}
	if _, ok := noteFile(t, x, out[:36]); !ok {
		t.Fatal("the new note must still be stored")
	}
	// retire an active note and an active fact
	if out, _ = call(t, st, "retire", map[string]any{"id": active[:8]}); strings.Contains(out, "deleted") {
		t.Fatalf("%q", out)
	}
	if out, _ = call(t, st, "retire", map[string]any{"id": fact[:8]}); strings.Contains(out, "deleted") {
		t.Fatalf("%q", out)
	}
	if _, err := os.Stat(filepath.Join(x.repo, "facts", "a", fact+".md")); err != nil {
		t.Fatal("a staged token retired a fact")
	}
	if _, ok := noteFile(t, x, active); !ok {
		t.Fatal("a staged token retired an active note")
	}
	// its own unverified notes it may replace and retire
	own, _ := call(t, st, "learn", map[string]any{"text": "A staged draft."})
	next, _ := call(t, st, "learn", map[string]any{"text": "A better staged draft.", "supersedes": own[:8]})
	if _, ok := noteFile(t, x, own); ok || !strings.Contains(next, "replaced") {
		t.Fatalf("an unverified note can be replaced: %q", next)
	}
	if out, _ = call(t, st, "retire", map[string]any{"id": next[:8]}); !strings.Contains(out, "deleted") {
		t.Fatalf("%q", out)
	}
}

// A staged token may replace or retire only unverified notes that carry its own name; an admin (non-staged) token any.
func TestStagedTokenCannotTouchAnotherAgentsNote(t *testing.T) {
	x := checkedSetup(t)
	st, rv := x.client(t, "st"), x.client(t, "rv")
	theirs, _ := call(t, rv, "learn", map[string]any{"text": "The reviewer's unverified finding."})
	out, isErr := call(t, st, "learn", map[string]any{"text": "The engineer overwrites it.", "supersedes": theirs[:8]})
	if isErr || !strings.Contains(out, "nothing was marked as replaced") || !strings.Contains(out, "another agent") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, _ = call(t, st, "retire", map[string]any{"id": theirs[:8]}); strings.Contains(out, "deleted") || !strings.Contains(out, "another agent") {
		t.Fatalf("%q", out)
	}
	before, ok := noteFile(t, x, theirs)
	if !ok || !strings.Contains(before, "author: reviewer\n") {
		t.Fatal("the reviewer's note changed or vanished")
	}
	if list, _ := call(t, x.client(t, "ver"), "list_memory", map[string]any{"status": "unverified"}); !strings.Contains(list, theirs[:8]) {
		t.Fatalf("the note is no longer listed:\n%s", list)
	}
	// its own note it may replace and retire
	own, _ := call(t, st, "learn", map[string]any{"text": "The engineer's own draft."})
	next, _ := call(t, st, "learn", map[string]any{"text": "The engineer's better draft.", "supersedes": own[:8]})
	if _, ok := noteFile(t, x, own); ok || !strings.Contains(next, "replaced") {
		t.Fatalf("%q", next)
	}
	if out, _ = call(t, st, "retire", map[string]any{"id": next[:8]}); !strings.Contains(out, "deleted") {
		t.Fatalf("%q", out)
	}
	// an admin token may do both to the reviewer's note
	adm := x.client(t, "wn")
	repl, _ := call(t, adm, "learn", map[string]any{"text": "Alice replaces the finding.", "supersedes": theirs[:8]})
	if _, ok := noteFile(t, x, theirs); ok || !strings.Contains(repl, "replaced") {
		t.Fatalf("%q", repl)
	}
	other, _ := call(t, rv, "learn", map[string]any{"text": "Another unverified finding."})
	if out, _ = call(t, adm, "retire", map[string]any{"id": other[:8]}); !strings.Contains(out, "deleted") {
		t.Fatalf("%q", out)
	}
}

func TestStagedFactAddIsRefused(t *testing.T) {
	x := checkedSetup(t)
	out, isErr := call(t, x.client(t, "st"), "fact_add", map[string]any{"text": "A staged fact."})
	if !isErr || !strings.Contains(out, "staged") {
		t.Fatalf("%v %q", isErr, out)
	}
	if m, _ := filepath.Glob(filepath.Join(x.repo, "facts", "a", "*.md")); len(m) != 0 {
		t.Fatalf("a refused fact left a file: %v", m)
	}
}

func TestVerifyNeedsTheRightAndOnlyRemovesTheMark(t *testing.T) {
	x := checkedSetup(t)
	id, _ := call(t, x.client(t, "st"), "learn", map[string]any{"text": "A note waiting for a check."})
	before, _ := noteFile(t, x, id)
	if out, isErr := call(t, x.client(t, "wn"), "verify", map[string]any{"id": id[:8]}); !isErr || !strings.Contains(out, "may not verify") {
		t.Fatalf("%v %q", isErr, out)
	}
	if now, _ := noteFile(t, x, id); now != before {
		t.Fatal("a refused verify changed the file")
	}
	ver := x.client(t, "ver")
	if out, isErr := call(t, ver, "verify", map[string]any{"id": id[:8]}); isErr || !strings.Contains(out, "verified") {
		t.Fatalf("%v %q", isErr, out)
	}
	now, _ := noteFile(t, x, id)
	if want := strings.Replace(before, "status: unverified\n", "", 1); now != want {
		t.Fatalf("got:\n%s\nwant:\n%s", now, want)
	}
	if out, _ := call(t, ver, "recall", map[string]any{"query": "waiting for a check", "layer": "memory"}); strings.Contains(out, "unverified") {
		t.Fatalf("the mark is still in recall:\n%s", out)
	}
	// already active, unknown, and an id of another project: nothing changes
	for _, c := range []struct{ id, project, want string }{{id[:8], "", "already"}, {"ffff0000", "", "no single"}, {"abcd", "b", "may not use project b"}} {
		out, _ := call(t, ver, "verify", map[string]any{"id": c.id, "project": c.project})
		if !strings.Contains(out, c.want) {
			t.Fatalf("%+v: %q", c, out)
		}
	}
	if again, _ := noteFile(t, x, id); again != now {
		t.Fatal("a second verify changed the file")
	}
}

func TestRescanGivesTheSameStatusAndAuthor(t *testing.T) {
	x := checkedSetup(t)
	call(t, x.client(t, "st"), "learn", map[string]any{"text": "A staged note for the rescan."})
	call(t, x.client(t, "wn"), "learn", map[string]any{"text": "An active note for the rescan."})
	rows := func(db *index.DB) string {
		r, err := db.Query(`SELECT id, status, author FROM nodes WHERE kind='note' AND project='a' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var b strings.Builder
		for r.Next() {
			var id, s, a string
			r.Scan(&id, &s, &a)
			b.WriteString(id + "|" + s + "|" + a + "\n")
		}
		return b.String()
	}
	fresh, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := fresh.Rescan(x.repo); err != nil {
		t.Fatal(err)
	}
	live, rebuilt := rows(x.srv.DB), rows(fresh)
	if live != rebuilt || !strings.Contains(live, "|unverified|scout") || !strings.Contains(live, "||alice") {
		t.Fatalf("live:\n%s\nrebuilt:\n%s", live, rebuilt)
	}
}
