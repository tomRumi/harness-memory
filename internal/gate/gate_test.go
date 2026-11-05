package gate

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const idA = "aaaaaaaa-1111-2222-3333-444444444444"
const idB = "bbbbbbbb-1111-2222-3333-444444444444"
const noteA = "The embedding service listens on port 8765 over loopback and must stay on 127.0.0.1 in production."
const noteB = "Rebuild the index with `hms rescan` after editing wiki/devops/modules/rag.md by hand, it takes 4.5 s."

func good(id, note string) string { return "- " + note + " [[" + id + "]]" }

type fixture struct {
	name     string
	replace  bool
	notes    []Note
	original string
	text     string
	wantOK   bool
}

func fixtures() []fixture {
	ab := []Note{{idA, noteA}, {idB, noteB}}
	section := good(idA, noteA) + "\n" + good(idB, noteB)
	orig := "# Entry\n\n## Setup\nThe daemon listens on port 8765 at http://127.0.0.1:8765/mcp and stores state in ~/.local/share/hms.\n\n## Notes\n```\nhms init\n```\nBuild takes 4.5 s on a 6 CPU machine.\n"
	one := func(n string) []Note { return []Note{{idA, n}} }
	return []fixture{
		{name: "faithful append", notes: ab, text: section, wantOK: true},
		{name: "wrapped lines and extra spaces", notes: ab, text: strings.ReplaceAll(good(idA, noteA), " over ", "\n  over  ") + "\n\n" + good(idB, noteB), wantOK: true},
		{name: "short note kept whole", notes: one("Use port 8765 only."), text: good(idA, "Use port 8765 only."), wantOK: true},
		{name: "short note paraphrased", notes: one("Use port 8765 only."), text: good(idA, "Only port 8765 is to be used."), wantOK: false},
		{name: "note missing from the section", notes: ab, text: good(idA, noteA), wantOK: false},
		{name: "section far too short", notes: ab, text: "- ports [[" + idA + "]] [[" + idB + "]]", wantOK: false},
		{name: "section far too long", notes: ab, text: section + "\n" + strings.Repeat("Padding words that say nothing at all. ", 20), wantOK: false},
		{name: "no citation for a note", notes: ab, text: noteA + "\n" + good(idB, noteB), wantOK: false},
		{name: "port 8765 becomes 8766", notes: ab, text: good(idA, strings.Replace(noteA, "8765", "8766", 1)) + "\n" + good(idB, noteB), wantOK: false},
		{name: "4.5 s becomes 5.4 s", notes: ab, text: good(idA, noteA) + "\n" + good(idB, strings.Replace(noteB, "4.5", "5.4", 1)), wantOK: false},
		{name: "port 80 becomes port 8080", notes: one("Serve the status page on port 80 and nothing else, never a second listener."), text: good(idA, "Serve the status page on port 8080 and nothing else, never a second listener."), wantOK: false},
		{name: "count 3 becomes 5", notes: one("The job gives up after 3 attempts and then waits for a person to clear the flag."), text: good(idA, "The job gives up after 5 attempts and then waits for a person to clear the flag."), wantOK: false},
		{name: "number on another note's item", notes: ab, text: good(idA, strings.Replace(noteA, "8765", "9000", 1)) + "\n" + good(idB, noteB+" It uses 8765."), wantOK: false},
		{name: "list as a paragraph", notes: one("- Restart the daemon after every config change\n- Check the health page before and after\n- Keep the old config for one day"), text: good(idA, "Restart the daemon after every config change; check the health page before and after; keep the old config for one day"), wantOK: true},
		{name: "replace: faithful rewrite", replace: true, original: orig, text: strings.Replace(orig, "stores state in", "keeps its state in", 1), wantOK: true},
		{name: "replace: heading lost", replace: true, original: orig, text: strings.Replace(orig, "## Notes\n", "", 1), wantOK: false},
		{name: "replace: fence removed", replace: true, original: orig, text: strings.Replace(orig, "```\nhms init\n```\n", "hms init\n", 1), wantOK: false},
		{name: "replace: port changed", replace: true, original: orig, text: strings.ReplaceAll(orig, "8765", "8766"), wantOK: false},
		{name: "replace: time dropped", replace: true, original: orig, text: strings.Replace(orig, "4.5 s", "a few seconds", 1), wantOK: false},
	}
}

func TestFixtureTable(t *testing.T) {
	for _, f := range fixtures() {
		var ok bool
		var probs []string
		if f.replace {
			ok, probs = CheckReplace(f.original, f.text)
		} else {
			ok, probs = CheckAppend(f.notes, f.text)
		}
		if ok != f.wantOK {
			t.Errorf("%s: got ok=%v, want %v (%v)", f.name, ok, f.wantOK, probs)
		}
	}
}

func TestNumbersAreWholeAndListNumbersAreIgnored(t *testing.T) {
	if got := missingNumbers("port 80 and 4.5 s", "port 8080 and 4.5 s"); len(got) != 1 || got[0] != "80" {
		t.Fatalf("%v", got)
	}
	if got := missingNumbers("1. one\n2. two\nvalue 7", "one two value 7"); len(got) != 0 {
		t.Fatalf("list numbers must not count: %v", got)
	}
	if got := missingNumbers("see 2026-11-05", "see 2026-11-06"); len(got) != 1 || got[0] != "05" {
		t.Fatalf("a changed date must be seen: %v", got)
	}
}

func TestCitationOnASourceSubBulletCountsForItsItem(t *testing.T) {
	n := Note{"bbbbbbbb-0000-0000-0000-000000000000", "The nightly job copies the dump to /srv/backup/hms and keeps 7 copies in total."}
	section := "- The nightly job copies the dump to /srv/backup/hms and keeps 7 copies in total.\n  - source [[bbbbbbbb]]\n"
	if ok, p := CheckAppend([]Note{n}, section); !ok {
		t.Fatalf("%v", p)
	}
}

func TestLongNotesAreCheckedInReasonableTime(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "Line %d says that value %d is 10.0.%d.%d on port %d and holds it.\n", i, i, i%200, i%250, 8000+i%900)
	}
	big := b.String()
	start := time.Now()
	CheckReplace(big, strings.ReplaceAll(big, "value", "item"))
	CheckAppend([]Note{{"cccccccc-0000-0000-0000-000000000000", big}}, "- unrelated text [[cccccccc]]")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("300 KB took %s", d)
	}
}
