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
