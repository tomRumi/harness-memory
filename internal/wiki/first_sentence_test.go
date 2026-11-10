package wiki

import "testing"

func TestFirstSentenceSkipsCommonMarkFences(t *testing.T) {
	for body, want := range map[string]string{
		"```\ncode here.\n```\nThe real text. More.\n":         "The real text.",
		"> ```\n> quoted code.\n> ```\nThe real text. More.\n": "The real text.",
	} {
		if got := FirstSentence(body); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
}
