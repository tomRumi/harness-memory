package gate

import (
	"fmt"
	"strings"
	"testing"
)

type fixture struct {
	name string
	// kind: "append" or "replace"
	kind     string
	notes    []Note
	original string
	text     string
	wantOK   bool
	// expectedMiss marks a case the gate cannot catch by design (meaning is not judged)
	expectedMiss bool
	// valueFlip marks a case where a hard value was changed: a pass there is a miss that must not happen
	valueFlip bool
}

const idA = "aaaaaaaa-1111-2222-3333-444444444444"
const idB = "bbbbbbbb-1111-2222-3333-444444444444"
const noteA = "The embedding service listens on port 8765 over loopback and must stay on 127.0.0.1 in production."
const noteB = "Rebuild the index with `hms rescan` after editing wiki/devops/modules/rag.md by hand, it takes 4.5 s."

func good(id, note string) string { return "- " + note + " [[" + id + "]]" }

func fixtures() []fixture {
	ab := []Note{{idA, noteA}, {idB, noteB}}
	section := good(idA, noteA) + "\n" + good(idB, noteB)
	orig := "# Entry\n\n## Setup\nThe daemon listens on port 8765 at http://127.0.0.1:8765/mcp and stores state in ~/.local/share/hms.\n\n## Notes\n```\nhms init\n```\nBuild takes 4.5 s on a 6 CPU machine.\n"
	return []fixture{
		{name: "faithful append", kind: "append", notes: ab, text: section, wantOK: true},
		{name: "reformatted append (wrapped lines, extra spaces)", kind: "append", notes: ab, text: strings.ReplaceAll(good(idA, noteA), " over ", "\n  over  ") + "\n\n" + strings.ReplaceAll(good(idB, noteB), "after editing", "after\n   editing"), wantOK: true},
		{name: "long note kept with small rewording elsewhere", kind: "append", notes: ab[:1], text: "- The embedding service listens on port 8765 over loopback and must stay on 127.0.0.1 in production, always. [[" + idA + "]]", wantOK: true},
		{name: "short note (4 words) kept whole", kind: "append", notes: []Note{{idA, "Use port 8765 only"}}, text: "- Use port 8765 only [[" + idA + "]]", wantOK: true},
		{name: "short note (4 words) paraphrased", kind: "append", notes: []Note{{idA, "Use port 8765 only"}}, text: "- Only port 8765 is to be used [[" + idA + "]]", wantOK: false},
		{name: "note missing from section", kind: "append", notes: ab, text: good(idA, noteA) + "\n- Something else entirely about rebuilding indexes in general terms here [[" + idB + "]]", wantOK: false},
		{name: "section too short", kind: "append", notes: ab, text: "- See notes [[" + idA + "]] [[" + idB + "]]", wantOK: false},
		{name: "section too long", kind: "append", notes: ab[:1], text: good(idA, noteA) + "\n" + strings.Repeat("Filler sentence that adds nothing at all. ", 12), wantOK: false},
		{name: "port flipped 8765 to 8766", kind: "append", notes: ab[:1], text: good(idA, strings.Replace(noteA, "8765", "8766", 1)), wantOK: false, valueFlip: true},
		{name: "address flipped to another private address", kind: "append", notes: ab[:1], text: good(idA, strings.Replace(noteA, "127.0.0.1", "10.0.0.7", 1)), wantOK: false, valueFlip: true},
		{name: "duration flipped 4.5 s to 5.4 s", kind: "append", notes: ab[1:], text: good(idB, strings.Replace(noteB, "4.5 s", "5.4 s", 1)), wantOK: false, valueFlip: true},
		{name: "command flipped in backticks", kind: "append", notes: ab[1:], text: good(idB, strings.Replace(noteB, "hms rescan", "hms rebuild", 1)), wantOK: false, valueFlip: true},
		{name: "path flipped", kind: "append", notes: ab[1:], text: good(idB, strings.Replace(noteB, "rag.md", "rug.md", 1)), wantOK: false, valueFlip: true},
		{name: "value present but not on the citing line", kind: "append", notes: ab[:1], text: "Port 8765 and 127.0.0.1 are mentioned here.\n- The embedding service listens on over loopback and must stay on in production. [[" + idA + "]]", wantOK: false},
		{name: "path gains a prefix", kind: "append", notes: []Note{{idA, "Backups are written below /data/backups/hms and rotated weekly by the job."}}, text: good(idA, "Backups are written below /mnt/data/backups/hms and rotated weekly by the job."), wantOK: false, valueFlip: true},
		{name: "path gains a suffix", kind: "append", notes: []Note{{idA, "The release is always served from /srv/app and never from a copy somewhere else."}}, text: good(idA, "The release is always served from /srv/app/current and never from a copy somewhere else."), wantOK: false, valueFlip: true},
		{name: "quoted value gains words", kind: "append", notes: []Note{{idA, "The page header must carry the exact robots value \"noindex\" on every staging page."}}, text: good(idA, "The page header must carry the exact robots value \"noindex, nofollow\" on every staging page."), wantOK: false, valueFlip: true},
		{name: "commit hash changes by one character", kind: "append", notes: []Note{{idA, "The regression was introduced in commit 3fe598d and fixed one day after that."}}, text: good(idA, "The regression was introduced in commit 3fe598e and fixed one day after that."), wantOK: false, valueFlip: true},
		{name: "short id changes by one character", kind: "append", notes: []Note{{idA, "Retire the duplicate note with the short id a91f3c07 before the next curation run."}}, text: good(idA, "Retire the duplicate note with the short id a91f3c08 before the next curation run."), wantOK: false, valueFlip: true},
		{name: "date changes by one day", kind: "append", notes: []Note{{idA, "The certificate expires on 2026-12-01 and must be renewed before that date arrives."}}, text: good(idA, "The certificate expires on 2026-12-07 and must be renewed before that date arrives."), wantOK: false, valueFlip: true},
		{name: "version gains a digit", kind: "append", notes: []Note{{idA, "Pin the library to 1.2.3 until the upstream fix for the parser is released."}}, text: good(idA, "Pin the library to 1.2.30 until the upstream fix for the parser is released."), wantOK: false, valueFlip: true},
		{name: "a count of 3 becomes 5", kind: "append", notes: []Note{{idA, "The job gives up after 3 attempts and then waits for a person to clear the flag."}}, text: good(idA, "The job gives up after 5 attempts and then waits for a person to clear the flag."), wantOK: false, valueFlip: true},
		{name: "the same list as a paragraph", kind: "append", notes: []Note{{idA, "- Restart the daemon after every config change\n- Check the health page before and after\n- Keep the old config for one day"}}, text: good(idA, "Restart the daemon after every config change; check the health page before and after; keep the old config for one day"), wantOK: true},
		{name: "sentences reordered", kind: "append", notes: []Note{{idA, "The cache is cleared nightly at 03:00. The warm-up job runs at 03:30 and fills it again."}}, text: good(idA, "The warm-up job runs at 03:30 and fills it again. The cache is cleared nightly at 03:00."), wantOK: true},
		{name: "port 80 becomes port 8080", kind: "append", notes: []Note{{idA, "Serve the status page on port 80 and nothing else, never a second listener."}}, text: good(idA, "Serve the status page on port 8080 and nothing else, never a second listener."), wantOK: false, valueFlip: true},
		{name: "address 10.0.0.1 becomes 10.0.0.12", kind: "append", notes: []Note{{idA, "The backup target is 10.0.0.1 and it accepts one connection at a time only."}}, text: good(idA, "The backup target is 10.0.0.12 and it accepts one connection at a time only."), wantOK: false, valueFlip: true},
		{name: "host with an unusual ending changed by one digit", kind: "append", notes: []Note{{idA, "Deploy to srv4410.corp.example first and check the logs there before touching anything."}}, text: good(idA, "Deploy to srv4411.corp.example first and check the logs there before touching anything."), wantOK: false, valueFlip: true},
		{name: "plain counts changed (10 notes, 3 attempts)", kind: "append", notes: []Note{{idA, "Merging starts at 10 notes and stops after 3 attempts until a person clears the flag."}}, text: good(idA, "Merging starts at 12 notes and stops after 5 attempts until a person clears the flag."), wantOK: false, valueFlip: true},
		{name: "same words rewrapped with a break inside '100 MB'", kind: "append", notes: []Note{{idA, "The upload limit is 100 MB per file and the server rejects anything larger than that."}}, text: good(idA, "The upload limit is 100\n  MB per file and the server rejects anything larger than that."), wantOK: true},
		{name: "one-word flip of a non-value word (must / must not)", kind: "append", notes: ab[:1], text: good(idA, strings.Replace(noteA, "must stay", "must not stay", 1)), wantOK: true, expectedMiss: true},
		{name: "replace: faithful rewrite", kind: "replace", original: orig, text: strings.Replace(orig, "stores state in", "keeps its state in", 1), wantOK: true},
		{name: "replace: heading lost", kind: "replace", original: orig, text: strings.Replace(orig, "## Notes", "## Remarks", 1), wantOK: false},
		{name: "replace: fence removed", kind: "replace", original: orig, text: strings.Replace(orig, "```\nhms init\n```\n", "hms init\n", 1), wantOK: false},
		{name: "replace: port changed", kind: "replace", original: orig, text: strings.ReplaceAll(orig, "8765", "8766"), wantOK: false, valueFlip: true},
		{name: "replace: path dropped", kind: "replace", original: orig, text: strings.Replace(orig, " and stores state in ~/.local/share/hms", "", 1), wantOK: false, valueFlip: true},
	}
}

// TestFixtureTable runs the 20 fixtures and records the two error rates the plan asks for.
func TestFixtureTable(t *testing.T) {
	var wrongReject, miss, valueMiss int
	var table []string
	for _, f := range fixtures() {
		var ok bool
		var problems []string
		if f.kind == "append" {
			ok, problems = CheckAppend(f.notes, f.text)
		} else {
			ok, problems = CheckReplace(f.original, f.text)
		}
		verdict := "pass"
		if !ok {
			verdict = "reject"
		}
		table = append(table, fmt.Sprintf("| %s | %s | %s |", f.name, map[bool]string{true: "pass", false: "reject"}[f.wantOK], verdict))
		switch {
		case f.wantOK && !ok:
			wrongReject++
			t.Logf("WRONG REJECTION %q: %v", f.name, problems)
		case !f.wantOK && ok:
			miss++
			if f.valueFlip {
				valueMiss++
			}
			t.Logf("MISS %q", f.name)
		case f.expectedMiss && ok:
			t.Logf("expected miss by design: %q", f.name)
		}
	}
	t.Logf("fixtures %d: wrong rejections %d, misses on hard-value flips %d, other misses %d\n%s", len(fixtures()), wrongReject, valueMiss, miss-valueMiss, strings.Join(table, "\n"))
	if len(fixtures()) < 20 {
		t.Fatalf("need at least 20 fixtures, have %d", len(fixtures()))
	}
	if wrongReject > 1 {
		t.Fatalf("%d wrong rejections (ceiling is 1 in 20)", wrongReject)
	}
	if valueMiss != 0 {
		t.Fatalf("%d hard-value flips were not caught (ceiling is 0)", valueMiss)
	}
	if miss != 0 {
		t.Fatalf("%d other fixtures passed that should be rejected", miss)
	}
}

func TestHardValuesCoverTheKinds(t *testing.T) {
	text := "Open https://example.org/a?b=1 on 10.1.2.3:8080, v2.1.0, 4.5 s, 3 GB, host db.example.com, `hms rescan`, \"exact words\", snake_case_name, camelCaseName, ~/.config/x and /var/log/y.log, build 20261104."
	got := strings.Join(HardValues(text), " | ")
	for _, want := range []string{"https://example.org/a?b=1", "10.1.2.3:8080", "v2.1.0", "4.5 s", "3 GB", "db.example.com", "hms rescan", "exact words", "snake_case_name", "camelCaseName", "~/.config/x", "/var/log/y.log", "20261104"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing hard value %q in %s", want, got)
		}
	}
}
