// Package chunk splits text into the pieces that are indexed and returned by recall.
package chunk

import (
	"strings"

	"github.com/tomRumi/harness-memory-service/internal/md"
)

// WikiCap is the most characters one chunk of a wiki section may hold; a longer section is cut.
const WikiCap = 4000

// Piece is one indexed chunk and the section heading it sits under.
type Piece struct {
	Section string
	Text    string
}

// Wiki splits markdown on level-two headings, so each "## " section stays whole. Text before the first
// heading is its own piece. A section over WikiCap is cut at the last blank line before the cap, or at
// the cap itself when there is none. Whitespace-only pieces are dropped.
func Wiki(body string) []Piece {
	body = md.NormalizeNL(body)
	var parts []Piece
	cur := Piece{}
	var sb strings.Builder
	flush := func() {
		cur.Text = sb.String()
		body := cur.Text
		if cur.Section != "" { // a heading with nothing under it carries no content
			if i := strings.IndexByte(body, '\n'); i >= 0 {
				body = body[i+1:]
			} else {
				body = ""
			}
		}
		if strings.TrimSpace(body) != "" {
			parts = append(parts, cur)
		}
		sb.Reset()
	}
	var fence md.Fence
	for _, line := range strings.SplitAfter(body, "\n") {
		if !fence.Line(line) && strings.HasPrefix(line, "## ") { // a "## " line inside a code block is code
			flush()
			cur = Piece{Section: strings.TrimSpace(strings.TrimPrefix(line, "## "))}
		}
		sb.WriteString(line)
	}
	flush()
	var out []Piece
	for _, p := range parts {
		for _, t := range capText(p.Text, WikiCap) {
			out = append(out, Piece{Section: p.Section, Text: t})
		}
	}
	return out
}

func capText(s string, limit int) []string {
	var out []string
	for len([]rune(s)) > limit {
		r := []rune(s)
		cut := limit
		if i := strings.LastIndex(string(r[:limit]), "\n\n"); i > limit/2 {
			cut = len([]rune(string(r[:limit])[:i]))
		}
		out = append(out, strings.TrimRight(string(r[:cut]), "\n"))
		s = strings.TrimLeft(string(r[cut:]), "\n")
	}
	if strings.TrimSpace(s) != "" {
		out = append(out, s)
	}
	return out
}
