// Package md reads and writes markdown files that carry optional YAML frontmatter.
package md

import (
	"bytes"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Doc is a markdown file split into its frontmatter and its body.
type Doc struct {
	FM    map[string]any
	Body  string
	Error string // set when a frontmatter block exists but cannot be parsed; the file is then read as plain body
}

// Parse splits src. A frontmatter block is a leading "---" line, YAML, and a closing "---" line.
// A malformed block never fails the read: the whole file becomes the body and Error says why.
func Parse(src []byte) Doc {
	s := string(bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n")))
	if !strings.HasPrefix(s, "---\n") {
		return Doc{Body: s}
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	trailing := false
	if end < 0 && strings.HasSuffix(rest, "\n---") {
		end, trailing = len(rest)-4, true
	}
	if end < 0 {
		return Doc{Body: s, Error: "frontmatter block is not closed"}
	}
	fm := map[string]any{}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return Doc{Body: s, Error: "frontmatter: " + firstLine(err.Error())}
	}
	body := ""
	if !trailing {
		body = rest[end+5:]
	}
	return Doc{FM: fm, Body: strings.TrimPrefix(body, "\n")}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Str returns a frontmatter value as text; non-text scalars are formatted by YAML.
func (d Doc) Str(key string) string {
	v, ok := d.FM[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	b, _ := yaml.Marshal(v)
	return strings.TrimSpace(string(b))
}

// List returns a frontmatter value as a list of strings (a single string becomes a one-item list).
func (d Doc) List(key string) []string {
	switch v := d.FM[key].(type) {
	case nil:
		return nil
	case string:
		if t := strings.TrimSpace(v); t != "" {
			return []string{t}
		}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// Bool returns true for a YAML true or the strings "true"/"yes".
func (d Doc) Bool(key string) bool {
	switch v := d.FM[key].(type) {
	case bool:
		return v
	case string:
		l := strings.ToLower(strings.TrimSpace(v))
		return l == "true" || l == "yes"
	}
	return false
}

// Render writes frontmatter (keys sorted, so output is deterministic) and body.
func Render(fm map[string]any, body string) []byte {
	var b bytes.Buffer
	if len(fm) > 0 {
		out, _ := yaml.Marshal(fm)
		b.WriteString("---\n")
		b.Write(out)
		b.WriteString("---\n\n")
	}
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.Bytes()
}

// Fence tracks fenced code blocks (``` and ~~~) line by line. A block opens with a run of three or more of one
// marker character and closes with a run of the same character that is at least as long.
type Fence struct {
	ch byte
	n  int
}

// Line feeds the next line and reports whether it belongs to a fenced block: the opening line, the lines inside and
// the closing line.
func (f *Fence) Line(line string) bool {
	t := strings.TrimSpace(line)
	if f.n == 0 {
		if c, n := fenceRun(t); n >= 3 {
			f.ch, f.n = c, n
			return true
		}
		return false
	}
	if c, n := fenceRun(t); c == f.ch && n >= f.n && strings.Trim(t, string(c)) == "" {
		f.ch, f.n = 0, 0
	}
	return true
}

// Open reports whether a fenced block is still open.
func (f *Fence) Open() bool { return f.n > 0 }

func fenceRun(t string) (byte, int) {
	if t == "" || (t[0] != '`' && t[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	return t[0], n
}

// IsFenceLine reports whether the line is a fence marker line (starts with ``` or ~~~).
func IsFenceLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// stripInlineCode blanks the inline code spans of one line (a run of backticks up to the next run of the same length).
func stripInlineCode(l string) string {
	if !strings.Contains(l, "`") {
		return l
	}
	var b strings.Builder
	for i := 0; i < len(l); {
		if l[i] != '`' {
			b.WriteByte(l[i])
			i++
			continue
		}
		j := i
		for j < len(l) && l[j] == '`' {
			j++
		}
		run := l[i:j]
		if k := strings.Index(l[j:], run); k >= 0 {
			// the closing run must not be part of a longer run
			end := j + k + len(run)
			if end >= len(l) || l[end] != '`' {
				b.WriteByte(' ')
				i = end
				continue
			}
		}
		b.WriteString(run)
		i = j
	}
	return b.String()
}

// WikiLinks returns the targets of [[wikilinks]] in text, without aliases or anchors, in order of first appearance.
// Links inside fenced code blocks and inline code spans are not links, and an embed (![[file.png]]) is not a link.
func WikiLinks(text string) []string {
	var keep []string
	var f Fence
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if f.Line(l) {
			continue
		}
		keep = append(keep, stripInlineCode(l))
	}
	text = strings.Join(keep, "\n")
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], "[[")
		if j < 0 {
			break
		}
		at := i + j
		start := at + 2
		k := strings.Index(text[start:], "]]")
		if k < 0 {
			break
		}
		t := text[start : start+k]
		i = start + k + 2
		if at > 0 && text[at-1] == '!' {
			continue
		}
		if p := strings.IndexAny(t, "|#"); p >= 0 {
			t = t[:p]
		}
		t = strings.TrimSpace(t)
		if t != "" && !strings.ContainsAny(t, "\n") && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
