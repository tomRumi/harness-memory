package curate

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Reviewer cases for CheckPage. want: true = the design wants it accepted.
func TestReviewCheckPageCases(t *testing.T) {
	note := Note{ID: "n1", Text: "The worker restarts after 5 failed health checks and then pages the on-call owner."}
	run := "The worker restarts after 5 failed health checks and then pages the on-call owner."
	old := "# Ops\n\nFirst line about the service.\nSecond line about the service.\n\n## Ports\n\nThe daemon listens on 8765.\n"
	none := func(string) bool { return false }
	all := func(string) bool { return true }
	only := func(ok ...string) func(string) bool {
		return func(l string) bool {
			for _, o := range ok {
				if strings.EqualFold(o, l) {
					return true
				}
			}
			return false
		}
	}
	long := strings.Repeat("A line of an old and long page that is kept as it is, with words.\n", 30000)
	cases := []struct {
		name     string
		old, new string
		notes    []Note
		resolve  func(string) bool
		want     bool
	}{
		{"C01 plain insert", old, old + "\n" + run + "\n", []Note{note}, none, true},
		{"C02 old line moved", old, "# Ops\n\nSecond line about the service.\nFirst line about the service.\n\n## Ports\n\nThe daemon listens on 8765.\n\n" + run + "\n", []Note{note}, none, false},
		{"C03 old line changed by one character", old, strings.Replace(old, "First line", "First lime", 1) + "\n" + run + "\n", []Note{note}, none, false},
		{"C04 whitespace inside an old line", old, strings.Replace(old, "First line", "First   line", 1) + "\n" + run + "\n", []Note{note}, none, true},
		{"C05 note number moved to another section", old, "# Ops\n\nFirst line about the service.\nSecond line about the service.\nThe worker restarts after failed health checks and then pages the on-call owner.\n\n## Ports\n\nThe daemon listens on 8765.\nRetries: 5\n", []Note{note}, none, false},
		{"C06 run of 8 words broken by a heading", old, old + "\nThe worker restarts after 5 failed\n\n## Health\n\nhealth checks and then pages the on-call owner.\n", []Note{note}, none, false},
		{"C07 duplicate headings, note under the second", "# A\n\n## Notes\n\nx one\n\n## Notes\n\ny two\n", "# A\n\n## Notes\n\nx one\n\n## Notes\n\ny two\n" + run + "\n", []Note{note}, none, true},
		{"C08 CRLF new body", old, strings.ReplaceAll(old+"\n"+run+"\n", "\n", "\r\n"), []Note{note}, none, true},
		{"C09 code fence holding a # line, note after it", old, old + "\n```bash\n# restart\nsystemctl restart w\n```\n" + run + "\n", []Note{note}, none, true},
		{"C10 bash [[ ]] inside a code fence", old, old + "\n" + run + "\n\n```bash\nif [[ -f /etc/w.conf ]]; then echo ok; fi\n```\n", []Note{note}, none, true},
		{"C11 note NFD, page NFC (cafe)", "", "# Cafe\n\nThe café machine needs descaling every 5 weeks or the pump fails.\n", []Note{{ID: "n2", Text: "The cafe\u0301 machine needs descaling every 5 weeks or the pump fails."}}, none, true},
		{"C12 straight quote in note, curly in page", "", "# W\n\nThe worker doesn’t restart after 5 failed checks unless the owner is paged.\n", []Note{{ID: "n3", Text: "The worker doesn't restart after 5 failed checks unless the owner is paged."}}, none, true},
		{"C11b short note NFD, page NFC", "", "# C\n\nThe Café opens at 7 every day.\n", []Note{{ID: "n7", Text: "Cafe\u0301 opens at 7."}}, none, true},
		{"C12b short note straight quote, page curly", "", "# W\n\nWe don’t deploy on Friday 13.\n", []Note{{ID: "n8", Text: "Don't deploy on Friday 13."}}, none, true},
		{"C12c short note with an em dash vs hyphen", "", "# W\n\nRetry window - 30 seconds.\n", []Note{{ID: "n9", Text: "Retry window — 30 seconds."}}, none, true},
		{"C13 very long page (2 MB) plus a note", long, long + run + "\n", []Note{note}, none, true},
		{"C14 old page with an odd ``` count (hand edit)", "# A\n\n```\nunclosed\n", "# A\n\n```\nunclosed\n" + run + "\n", []Note{note}, none, true},
		{"C15 1,000 written as 1000", "", "# R\n\nThe limit is 1000 requests per minute for each key we issue.\n", []Note{{ID: "n4", Text: "The limit is 1,000 requests per minute for each key we issue."}}, none, false},
		{"C16 number present in section but only inside a bigger number", "", "# P\n\nThe proxy port is 8080 and it forwards to the app server quickly.\n", []Note{{ID: "n5", Text: "The proxy port is 80 and it forwards to the app server quickly."}}, none, false},
		{"C17 short note whole inside a sentence", "", "# P\n\nIn production the port is 8765.\n", []Note{{ID: "n6", Text: "Port is 8765."}}, none, true},
		{"C18 new link to a missing page", old, old + "\n" + run + " See [[Nowhere]].\n", []Note{note}, none, false},
		{"C19 Obsidian embed of an attachment", old, old + "\n" + run + "\n![[diagram.png]]\n", []Note{note}, only("Ports"), true},
		{"C20 table separator respaced", "# T\n\n| a | b |\n|---|---|\n| 1 | 2 |\n", "# T\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n" + run + "\n", []Note{note}, none, false},
		{"C21 old paragraph line rewrapped into two lines", old, strings.Replace(old, "First line about the service.", "First line about\nthe service.", 1) + "\n" + run + "\n", []Note{note}, none, false},
		{"C22 heading inside a ~~~ fence splits the section", "", "# W\n\n" + "The worker restarts after failed health checks and then pages the on-call owner.\n~~~\n# 5 is the limit\n~~~\nlimit 5\n", []Note{note}, all, true},
		{"C23 heading of a section changed level", old, strings.Replace(old, "## Ports", "### Ports", 1) + "\n" + run + "\n", []Note{note}, none, false},
		{"C24 note moved into a list item with markdown bold", old, old + "\n- **The worker** restarts after 5 failed health checks and then pages the on-call owner.\n", []Note{note}, none, true},
		{"C25 the note's 8-word run present, the rest dropped (number kept)", old, old + "\nThe worker restarts after 5 failed health checks.\n", []Note{note}, none, true},
	}
	for _, c := range cases {
		start := time.Now()
		probs := CheckPage(c.old, c.new, c.notes, c.resolve)
		got := len(probs) == 0
		verdict := "ok"
		if got != c.want {
			verdict = "MISMATCH"
		}
		t.Logf("%-62s accepted=%-5v design=%-5v %s %v %s", c.name, got, c.want, verdict, time.Since(start).Round(time.Millisecond), strings.Join(probs, "; "))
		if got != c.want {
			t.Errorf("%s: accepted=%v, the design wants %v (%s)", c.name, got, c.want, strings.Join(probs, "; "))
		}
	}
}

// C26: links between new pages of one submission, where the target fails; the linking page must be refused too.
func TestReviewCheckChainedLinks(t *testing.T) {
	st, _ := newStore(t)
	ns := learnN(t, st, proj, 3, tmplWorker)
	rep, err := Submit(context.Background(), st, Submission{Project: proj, Pages: []Page{
		{Title: "A", Summary: "a", Body: "# A\n\nSee [[B]].\n\n" + bullets(ns[:1]), Notes: ids(ns[:1])},
		{Title: "B", Summary: "b", Body: "# B\n\nSee [[C]].\n\n" + bullets(ns[1:2]), Notes: ids(ns[1:2])},
		{Title: "C", Summary: "c", Body: "# C\n\nnothing of the note\n", Notes: ids(ns[2:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rep.Pages {
		t.Logf("%s %s %v", p.Title, p.Status, p.Problems)
	}
	if exists(st, "wiki/proj/A.md") || exists(st, "wiki/proj/B.md") {
		t.Errorf("DEFECT a page links (through a chain) to a page that failed, and was written")
	}
}
