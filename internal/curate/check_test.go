package curate

import (
	"fmt"
	"strings"
	"testing"
)

func noteOf(id, text string) Note { return Note{ID: id, Text: text} }

func yes(string) bool { return true }

// pageWith returns old plus the text of the notes as list items.
func pageWith(old string, ns ...Note) string {
	var b strings.Builder
	b.WriteString(old)
	for _, n := range ns {
		b.WriteString("- " + n.Text + "\n")
	}
	return b.String()
}

const (
	nPort   = "The embedding service listens on port 8765 over loopback and must stay on 127.0.0.1 in production."
	nBuild  = "Rebuild the index with `hms rescan` after editing wiki files by hand, it takes 4.5 s."
	nShort  = "Use port 80 only."
	nShort2 = "Never commit secrets."
)

func TestCheckPageTable(t *testing.T) {
	oldBody := "# Ports\n\nThe daemon uses a port.\n\n## Build\n\nBuilds are quick.\n"
	port, build := noteOf("aaaaaaaa-1", nPort), noteOf("bbbbbbbb-1", nBuild)
	short, short2 := noteOf("cccccccc-1", nShort), noteOf("dddddddd-1", nShort2)
	insertPorts := "# Ports\n\nThe daemon uses a port.\n- " + nPort + "\n\n## Build\n\nBuilds are quick.\n- " + nBuild + "\n"
	cases := []struct {
		name     string
		old, new string
		notes    []Note
		resolve  func(string) bool
		wantRule string // "" = pass; otherwise a substring of one problem
	}{
		{name: "new page, faithful", new: pageWith("# Ports\n\n", port, build), notes: []Note{port, build}},
		{name: "existing page, notes inserted in sections", old: oldBody, new: insertPorts, notes: []Note{port, build}},
		{name: "extra blank lines and trailing spaces are fine", old: oldBody, new: strings.ReplaceAll(insertPorts, "\n\n", "\n\n\n\n") + "   \n", notes: []Note{port, build}},
		{name: "old line indented differently is the same line", old: "- keep   this\n", new: "-  keep this   \n- " + nPort + "\n", notes: []Note{port}},

		{name: "insert only: line removed", old: oldBody, new: strings.Replace(insertPorts, "The daemon uses a port.\n", "", 1), notes: []Note{port, build}, wantRule: "insert only"},
		{name: "insert only: lines reordered", old: oldBody, new: "## Build\n\nBuilds are quick.\n# Ports\n\nThe daemon uses a port.\n- " + nPort + "\n- " + nBuild + "\n", notes: []Note{port, build}, wantRule: "insert only"},
		{name: "insert only: line reworded", old: oldBody, new: strings.Replace(insertPorts, "Builds are quick.", "Builds are fast.", 1), notes: []Note{port, build}, wantRule: "insert only"},

		{name: "note: run of 8 words survives a rewrap and punctuation", new: "Port notes:\n\nThe embedding service listens\non port 8765 over loopback, and must stay on 127.0.0.1 in production!\n", notes: []Note{port}},
		{name: "note: only the first 7 words are kept", new: "The embedding service listens on port 8765 and then something else entirely.\n", notes: []Note{port}, wantRule: "is not in the page"},
		{name: "note: missing", new: pageWith("# P\n\n", port), notes: []Note{port, build}, wantRule: "note bbbbbbbb"},
		{name: "note: a list written as a paragraph keeps its runs", new: "Restart the daemon after every config change; check the health page before and after.\n", notes: []Note{noteOf("eeeeeeee-1", "- Restart the daemon after every config change\n- check the health page before and after")}},

		{name: "short note, whole", new: pageWith("# P\n\n", short), notes: []Note{short}},
		{name: "short note, paraphrased", new: "Only port 80 is to be used.\n", notes: []Note{short}, wantRule: "is not in the page"},
		{name: "short note, whole but inside a longer number", new: "Use port 8080 only.\n", notes: []Note{short}, wantRule: "is not in the page"},
		{name: "two short notes", new: pageWith("# P\n\n", short, short2), notes: []Note{short, short2}},

		{name: "numbers: 8765 becomes 8766", new: pageWith("# P\n\n", noteOf("x", strings.Replace(nPort, "8765", "8766", 1))), notes: []Note{port}, wantRule: "number 8765"},
		{name: "numbers: 80 becomes 8080", new: "Serve the status page on port 8080 and nothing else, never a second listener at all, ever.\n", notes: []Note{noteOf("ffffffff-1", "Serve the status page on port 80 and nothing else, never a second listener at all, ever.")}, wantRule: "number 80"},
		{name: "numbers: 4.5 becomes 5.4", new: pageWith("# P\n\n", noteOf("x", strings.Replace(nBuild, "4.5", "5.4", 1))), notes: []Note{build}, wantRule: "number 4.5"},
		{name: "numbers: moved to another section", new: "# A\n\nThe embedding service listens on port and must stay on loopback in production.\n\n# B\n\nIt is 8765 and 127.0.0.1 here\n", notes: []Note{noteOf("gggggggg-1", "The embedding service listens on port and must stay on loopback in production 8765")}, wantRule: "number 8765"},
		{name: "numbers: list numbers of a note are ignored", new: "Do these steps in order: first stop the daemon, then copy the files, then start again.\n", notes: []Note{noteOf("iiiiiiii-1", "1. first stop the daemon\n2. then copy the files\n3. then start again")}},
		{name: "numbers: a heading inside a fence is not a section break", new: "# A\n\n```\n# comment\n```\nThe embedding service listens on port 8765 over loopback and must stay on 127.0.0.1 in production.\n", notes: []Note{port}},

		{name: "links: broken", new: pageWith("# P\n\nSee [[Nowhere]].\n", port), notes: []Note{port}, resolve: func(l string) bool { return l == "Real" }, wantRule: "broken link [[Nowhere]]"},
		{name: "links: resolves", new: pageWith("# P\n\nSee [[Real|the page]] and [[Real#Head]].\n", port), notes: []Note{port}, resolve: func(l string) bool { return l == "Real" }},

		{name: "fences: even", new: "```\ncode\n```\n" + nPort + "\n", notes: []Note{port}},
		{name: "fences: odd", new: "```\ncode\n" + nPort + "\n", notes: []Note{port}, wantRule: "odd"},

		{name: "length: runaway", new: nPort + "\n" + strings.Repeat("Padding words that say nothing at all.\n", 100), notes: []Note{port}, wantRule: "too long"},
		{name: "length: within old + 3 x notes + 2000", new: nPort + "\n" + strings.Repeat("Padding words that say nothing.\n", 40), notes: []Note{port}},
		{name: "length: shorter than before", old: "line one\nline two\n", new: "line one\n", wantRule: "shorter than before"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.resolve
			if r == nil {
				r = yes
			}
			probs := CheckPage(c.old, c.new, c.notes, r)
			if c.wantRule == "" {
				if len(probs) != 0 {
					t.Fatalf("expected pass, got %v", probs)
				}
				return
			}
			for _, p := range probs {
				if strings.Contains(p, c.wantRule) {
					return
				}
			}
			t.Fatalf("expected a problem containing %q, got %v", c.wantRule, probs)
		})
	}
}

func TestCheckPageLargePageIsFast(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "Line %d of a long page that talks about unrelated things such as item %d and more.\n", i, i)
	}
	big := b.String()
	var notes []Note
	for i := 0; i < 40; i++ {
		notes = append(notes, noteOf(fmt.Sprintf("n%07d-1", i), fmt.Sprintf("Fresh observation number %d says that queue %d must drain before the nightly restart begins.", i, i)))
	}
	nb := big + pageWith("", notes...)
	if probs := CheckPage(big, nb, notes, yes); len(probs) != 0 {
		t.Fatal(probs)
	}
}

func TestEnumerationLabelsAreNotValues(t *testing.T) {
	got := numbers("Patterns: (1) fast probes, (2) typed results, 3) strict parsing, and 6 retries in 10 s.")
	want := []string{"6", "10"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOldBrokenLinksAreNotHeldAgainstAMerge(t *testing.T) {
	old := "# Page\n\nRelated: [[Gone Page]].\n"
	notes := []Note{{ID: "11111111-0000-0000-0000-000000000000", Text: "The worker retries three times and then parks the card for a person to look at."}}
	none := func(string) bool { return false }
	nw := old + "\nThe worker retries three times and then parks the card for a person to look at.\n"
	if p := CheckPage(old, nw, notes, none); len(p) != 0 {
		t.Fatalf("an old broken link must not refuse the page: %v", p)
	}
	nw2 := nw + "\nSee also [[Another Gone Page]].\n"
	if p := CheckPage(old, nw2, notes, none); len(p) != 1 || !strings.Contains(p[0], "Another Gone Page") {
		t.Fatalf("a new broken link must refuse the page: %v", p)
	}
}
