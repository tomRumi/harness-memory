// Package chunk splits text into the pieces that are indexed and returned by recall.
package chunk

import "strings"

const (
	WikiCap    = 4000    // characters; a wiki section longer than this is cut
	CodeWindow = 1500    // characters per code window
	CodeOver   = 150     // characters of overlap between code windows
	CodeCap    = 4500    // hard cap for any single code chunk
	CodeFile   = 200_000 // characters read from one file
)

// Piece is one indexed chunk and the section heading it sits under.
type Piece struct {
	Section string
	Text    string
}

// Wiki splits markdown on level-two headings, so each "## " section stays whole. Text before the first
// heading is its own piece. A section over WikiCap is cut at the last blank line before the cap, or at
// the cap itself when there is none. Whitespace-only pieces are dropped.
func Wiki(body string) []Piece {
	body = strings.ReplaceAll(body, "\r\n", "\n")
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
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.HasPrefix(line, "## ") {
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

// Code splits source text into line-aligned windows of about CodeWindow characters that overlap by about
// CodeOver characters. It reads at most CodeFile characters. No chunk exceeds CodeCap.
// This approximates the windowing of the service it replaces; it does not reproduce its splits.
func Code(text string) []Piece {
	r := []rune(strings.ReplaceAll(text, "\r\n", "\n"))
	if len(r) > CodeFile {
		r = r[:CodeFile]
	}
	if strings.TrimSpace(string(r)) == "" {
		return nil
	}
	lines := strings.SplitAfter(string(r), "\n")
	var out []Piece
	var cur []string
	size := 0
	emit := func() {
		if t := strings.Join(cur, ""); strings.TrimSpace(t) != "" {
			for _, c := range capText(t, CodeCap) {
				out = append(out, Piece{Text: c})
			}
		}
	}
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		if size+len([]rune(ln)) > CodeWindow && len(cur) > 0 {
			emit()
			// keep a tail of whole lines worth about CodeOver characters
			var tail []string
			n := 0
			for i := len(cur) - 1; i >= 0 && n < CodeOver; i-- {
				tail = append([]string{cur[i]}, tail...)
				n += len([]rune(cur[i]))
			}
			if n > CodeWindow/2 { // an oversized line must not be carried into the next window
				tail, n = nil, 0
			}
			cur, size = tail, n
		}
		cur = append(cur, ln)
		size += len([]rune(ln))
	}
	emit()
	return out
}
