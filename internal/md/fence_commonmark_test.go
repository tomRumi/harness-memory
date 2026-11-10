package md

import (
	"strings"
	"testing"
)

// Fences as CommonMark has them: one function for the check, the link finder, the chunker and the summary.
func TestFenceFollowsCommonMark(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"``` in ````", "````\n```\n[[A]]\n```\n````\n[[B]]\n", "B"},
		{"~~~ in ```", "```\n~~~\n[[A]]\n~~~\n```\n[[B]]\n", "B"},
		{"4-space indented ``` is code, not a fence", "text\n\n    ```\n[[Nowhere]]\n    ```\n", "Nowhere"},
		{"3-space indented ``` is a fence", "text\n\n   ```\n[[A]]\n   ```\n[[B]]\n", "B"},
		{"fence in a list item", "- item\n  ```\n  [[A]]\n  ```\n- [[B]]\n", "B"},
		{"CRLF", "```\r\n[[A]]\r\n```\r\n[[B]]\r\n", "B"},
		{"close with an info string is no close", "```go\n[[A]]\n```go\n[[B]]\n```\n[[C]]\n", "C"},
		{"backtick in the info string: not a fence", "```a`b\n[[Nowhere]]\n", "Nowhere"},
		{"tilde fence may have a backtick in the info string", "~~~a`b\n[[A]]\n~~~\n[[B]]\n", "B"},
		{"fence in a blockquote", "> ```\n> [[A]]\n> ```\n[[B]]\n", "B"},
		{"tab-indented ```", "text\n\n\t```\n[[Nowhere]]\n\t```\n", "Nowhere"},
		{"unclosed fence hides to the end", "```\n[[A]]\n", ""},
		{"~~~~ closed by ~~~ is no close", "~~~~\n[[A]]\n~~~\n[[B]]\n~~~~\n[[C]]\n", "C"},
	}
	for _, c := range cases {
		if got := strings.Join(WikiLinks(c.text), ","); got != c.want {
			t.Errorf("%s: links %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFenceOpenAtTheEnd(t *testing.T) {
	for text, want := range map[string]bool{
		"```\ncode\n":          true,
		"```\ncode\n```\n":     false,
		"    ```\ncode\n":      false,
		"> ```\n> code\n":      true,
		"```a`b\ncode\n":       false,
		"~~~\n```\n~~~\n":      false,
		"```\n    ```\ncode\n": true, // an indented ``` does not close
	} {
		var f Fence
		for _, l := range strings.Split(text, "\n") {
			f.Line(l)
		}
		if f.Open() != want {
			t.Errorf("%q: open=%v, want %v", text, f.Open(), want)
		}
	}
}
