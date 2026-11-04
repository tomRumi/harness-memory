package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWikiSplitsOnLevelTwoHeadings(t *testing.T) {
	got := Wiki("# Title\n\nintro\n\n## One\nfirst\n\n### sub\nstill one\n\n## Two\nsecond\n")
	if len(got) != 3 {
		t.Fatalf("want 3 pieces, got %d: %+v", len(got), got)
	}
	if got[0].Section != "" || got[1].Section != "One" || got[2].Section != "Two" {
		t.Fatalf("sections: %+v", got)
	}
	if !strings.Contains(got[1].Text, "still one") {
		t.Fatal("a level-three heading must stay inside its section")
	}
}

func TestWikiCapsAndDropsEmpty(t *testing.T) {
	long := "## Big\n" + strings.Repeat("word ", 3000) // ~15000 chars, no blank line
	got := Wiki(long + "\n\n## Empty\n   \n")
	if len(got) < 4 {
		t.Fatalf("expected a forced cut into several pieces, got %d", len(got))
	}
	for _, p := range got {
		if utf8.RuneCountInString(p.Text) > WikiCap {
			t.Fatalf("piece over cap: %d", utf8.RuneCountInString(p.Text))
		}
		if strings.TrimSpace(p.Text) == "" {
			t.Fatal("empty piece kept")
		}
	}
	for _, p := range got {
		if p.Section == "Empty" {
			t.Fatal("whitespace-only section must be dropped")
		}
	}
}

func TestCodeWindowsRespectCapsAndOverlap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		sb.WriteString("line number ")
		sb.WriteString(strings.Repeat("x", 40))
		sb.WriteString("\n")
	}
	got := Code(sb.String())
	if len(got) < 5 {
		t.Fatalf("expected several windows, got %d", len(got))
	}
	for _, p := range got {
		if n := utf8.RuneCountInString(p.Text); n > CodeCap || n > CodeWindow+200 {
			t.Fatalf("window too large: %d", n)
		}
	}
	// consecutive windows share their boundary line
	a, b := got[0].Text, got[1].Text
	last := a[strings.LastIndex(strings.TrimRight(a, "\n"), "\n")+1:]
	if !strings.Contains(b, strings.TrimRight(last, "\n")) {
		t.Fatal("no overlap between consecutive windows")
	}
}

func TestCodeHandlesOneHugeLineAndFileLimit(t *testing.T) {
	huge := strings.Repeat("a", 20000) + "\nshort\nlines\n"
	for _, p := range Code(huge) {
		if utf8.RuneCountInString(p.Text) > CodeCap {
			t.Fatal("huge line not cut")
		}
	}
	big := strings.Repeat("0123456789\n", 40000) // 440k chars
	total := 0
	for _, p := range Code(big) {
		total += utf8.RuneCountInString(p.Text)
	}
	if total > CodeFile*2 { // overlap may repeat a little, never double the file limit
		t.Fatalf("file limit not applied: %d", total)
	}
	if Code("   \n\n") != nil {
		t.Fatal("blank file must produce no chunks")
	}
}
