package chunk

import "testing"

// A "## " line inside a code block is code, not a heading: it must not split the section.
func TestRev3_HeadingInsideACodeBlockDoesNotSplit(t *testing.T) {
	body := "## Setup\nIntro text.\n\n```\n## not a heading\nmore code\n```\n\nAfter the code.\n\n## Next\nSecond section.\n"
	got := Wiki(body)
	if len(got) != 2 || got[0].Section != "Setup" || got[1].Section != "Next" {
		t.Fatalf("want 2 pieces (Setup, Next), got %+v", got)
	}
}
