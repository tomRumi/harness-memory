// Package gate is the integrity check that curated text must pass before it is committed. Nothing in it is
// model-based: it counts and compares. It does not judge meaning; its job is that no number, name or path is lost.
//
// Two checks share the extraction of "hard values":
//   - CheckAppend: a section written from notes (the automatic path). Every note must survive in it, and every hard
//     value of a note must appear in the list item or paragraph that cites that note.
//   - CheckReplace: new text for an existing entry (the `merge --rewrite` path). Headings, code fences and every hard
//     value of the old text must survive, and the length must stay within bounds.
//
// The note test is a port of the predecessor (consolidate.py:106-128) with its one known fault removed: a note
// shorter than eight words used to fail always; here such a note must appear whole.
package gate

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	LenMin = 0.60 // bounds of the new text relative to the text it replaces or summarises
	LenMax = 1.60
	Window = 8 // consecutive words of a note that must survive verbatim
)

var (
	spaces  = regexp.MustCompile(`\s+`)
	h2      = regexp.MustCompile(`(?m)^## .*$`)
	fenceRe = regexp.MustCompile("(?m)^\\s*```")

	// hard values: things that are wrong if one character changes
	reURL      = regexp.MustCompile(`https?://[^\s)>\]"']+`)
	rePath     = regexp.MustCompile(`(?:~|\.{1,2})?/[A-Za-z0-9._@-]+(?:/[A-Za-z0-9._@-]+)*|\b[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+\b`)
	reIP       = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{2,5})?\b`)
	reVersion  = regexp.MustCompile(`\bv?\d+(?:\.\d+){1,}(?:-[A-Za-z0-9.]+)?\b`)
	reUnit     = regexp.MustCompile(`\b\d[\d,]*(?:\.\d+)?\s?(?:%|ms|s|sec|min|h|d|GB|MB|KB|TB|GiB|MiB|KiB|x|px|rem|k|M|B)\b`)
	rePort     = regexp.MustCompile(`(?i)\bport\s+\d{2,5}\b|:\d{2,5}\b`)
	reHost     = regexp.MustCompile(`\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|net|org|io|hu|dev|app|ai|eu|local|sh|co|ch|de)\b`)
	reIdent    = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+\b|\b[a-z]+[A-Z][A-Za-z0-9]*\b`)
	reBacktick = regexp.MustCompile("`([^`\n]{2,80})`")
	reQuoted   = regexp.MustCompile(`"([^"\n]{3,80})"`)
	reNumber   = regexp.MustCompile(`\b\d{3,}\b`)
	reCite     = regexp.MustCompile(`\[\[[^\]]*\]\]`)
)

// body is a section without its citations, whose length counts as content nowhere.
func body(s string) string { return reCite.ReplaceAllString(s, "") }

// flat collapses every run of white space to one space, so a rewrapped line still contains the same value.
func flat(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

func norm(s string) string {
	return strings.TrimSpace(spaces.ReplaceAllString(strings.ToLower(s), " "))
}

// HardValues lists the values of text that must not be lost, without duplicates, in order of first appearance.
func HardValues(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.Trim(strings.TrimSpace(v), ".,;:!?")
		if len(v) < 2 || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	text = strings.ReplaceAll(text, "[[", " ")
	for _, re := range []*regexp.Regexp{reURL, reIP, rePort, reVersion, reUnit, reHost, reIdent, rePath, reNumber} {
		for _, m := range re.FindAllString(text, -1) {
			add(m)
		}
	}
	for _, re := range []*regexp.Regexp{reBacktick, reQuoted} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
	}
	return out
}

// Note is one source note of a merge.
type Note struct{ ID, Text string }

// CheckAppend checks a section written from notes. Every note must survive in it (some verbatim run of Window words,
// or the whole note when it is shorter than that), the section, without its citations, must be 0.60 to 1.60 times the notes'
// combined length,
// and every hard value of a note must appear in a line that cites the note's id.
func CheckAppend(notes []Note, section string) (ok bool, problems []string) {
	total := 0
	for _, n := range notes {
		total += len([]rune(n.Text))
	}
	if total > 0 {
		lo, hi := LenMin*float64(total), LenMax*float64(total)
		if got := float64(len([]rune(flat(body(section))))); got < lo || got > hi {
			problems = append(problems, fmt.Sprintf("section length %d outside [%d,%d] of the notes' %d", int(got), int(lo), int(hi), total))
		}
	}
	sn := norm(section)
	units := blocks(section)
	for _, n := range notes {
		words := strings.Split(norm(n.Text), " ")
		found := false
		if len(words) < Window {
			found = strings.Contains(sn, norm(n.Text))
		}
		for s := 0; !found && s+Window <= len(words); s++ {
			found = strings.Contains(sn, strings.Join(words[s:s+Window], " "))
		}
		if !found {
			problems = append(problems, fmt.Sprintf("note %s does not survive in the section (no verbatim run of %d words, or not whole when shorter)", short(n.ID), Window))
		}
		var cited []string
		for _, l := range units {
			if strings.Contains(l, n.ID) || (len(n.ID) >= 8 && strings.Contains(l, n.ID[:8])) {
				cited = append(cited, l)
			}
		}
		if len(cited) == 0 {
			problems = append(problems, fmt.Sprintf("no list item or paragraph cites note %s", short(n.ID)))
			continue
		}
		cl := flat(strings.Join(cited, "\n"))
		for _, v := range HardValues(n.Text) {
			if !strings.Contains(cl, flat(v)) {
				problems = append(problems, fmt.Sprintf("note %s: value %q is missing from the item that cites it", short(n.ID), v))
			}
		}
	}
	return len(problems) == 0, problems
}

// CheckReplace checks new text for an entry against the text it replaces: no heading lost, code fences balanced and
// unchanged in number, length within bounds, and every hard value of the old text still present.
func CheckReplace(original, replacement string) (ok bool, problems []string) {
	have := map[string]bool{}
	for _, h := range h2.FindAllString(replacement, -1) {
		have[h] = true
	}
	for _, h := range h2.FindAllString(original, -1) {
		if !have[h] {
			problems = append(problems, fmt.Sprintf("heading lost: %q", h))
		}
	}
	lo, hi := LenMin*float64(len([]rune(original))), LenMax*float64(len([]rune(original)))
	if n := float64(len([]rune(replacement))); n < lo || n > hi {
		problems = append(problems, fmt.Sprintf("length %d outside [%d,%d] of %d", int(n), int(lo), int(hi), len([]rune(original))))
	}
	fo, fn := len(fenceRe.FindAllString(original, -1)), len(fenceRe.FindAllString(replacement, -1))
	if fo != fn || fn%2 != 0 {
		problems = append(problems, fmt.Sprintf("fence count %d->%d (changed or odd)", fo, fn))
	}
	for _, v := range HardValues(original) {
		if !strings.Contains(replacement, v) {
			problems = append(problems, fmt.Sprintf("value %q is missing from the new text", v))
		}
	}
	return len(problems) == 0, problems
}

var bulletStart = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s`)

// blocks splits a section into list items and paragraphs: a block starts at a bullet or after a blank line and runs
// until the next one, so a bullet that wraps over several lines is one unit.
func blocks(section string) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, l := range strings.Split(section, "\n") {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		if bulletStart.MatchString(l) {
			flush()
		}
		cur = append(cur, l)
	}
	flush()
	return out
}

func short(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
