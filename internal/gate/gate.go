// Package gate is the check that curated text must pass before it is committed. It is a port of the predecessor's
// validate() (consolidate.py:106-128) with its one known fault removed, plus one addition: a number that is in a note
// must still be in the text that replaces it.
//
// Nothing in it is model-based and it does not judge meaning. Curation only appends, notes are superseded and never
// deleted, and everything is in git, so a bad merge can always be recovered; the check exists to stop the common
// accident (a note dropped, a section that is far too short, a fact reduced to a wish), not to defend against anything.
//
//   - CheckAppend: a section written from notes (the automatic path). Every note must survive in it, the length must
//     be in bounds, and the numbers of a note must be in the list item or paragraph that cites it.
//   - CheckReplace: new text for an existing entry (the `merge --rewrite` path). No heading lost, code fences
//     balanced and unchanged in number, length in bounds, and every number of the old text still present.
//
// A note shorter than eight words used to fail the predecessor's check always; here such a note must appear whole.
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
	spaces     = regexp.MustCompile(`\s+`)
	h2         = regexp.MustCompile(`(?m)^## .*$`)
	fenceRe    = regexp.MustCompile("(?m)^\\s*```")
	reNumber   = regexp.MustCompile(`\d+(?:[.,]\d+)*`)
	reListMark = regexp.MustCompile(`(?m)^\s*\d+[.)]\s`) // "1. " at the start of a line numbers a list; it is not a value
	reCite     = regexp.MustCompile(`\[\[[^\]]*\]\]`)
	bulletLine = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s`)
)

// body is a section without its citations.
func body(s string) string { return reCite.ReplaceAllString(s, "") }

func flat(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// norm lowercases and drops punctuation and list markers, so a list written as a paragraph keeps its word runs.
func norm(s string) string {
	s = strings.ToLower(reListMark.ReplaceAllString(s, ""))
	var b strings.Builder
	for _, r := range s {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127 {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return flat(b.String())
}

// numbers lists the numbers of a text, whole ("80" is not found in "8080"), without list numbers.
func numbers(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range reNumber.FindAllString(reListMark.ReplaceAllString(reCite.ReplaceAllString(text, ""), ""), -1) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// missingNumbers lists the numbers of old that new does not hold.
func missingNumbers(old, new string) []string {
	have := map[string]bool{}
	for _, n := range reNumber.FindAllString(new, -1) {
		have[n] = true
	}
	var out []string
	for _, n := range numbers(old) {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

// Note is one source note of a merge.
type Note struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// CheckAppend checks a section written from notes. Every note must survive in it (some verbatim run of Window words,
// or the whole note when it is shorter than that), the section, without its citations, must be 0.60 to 1.60 times the
// notes' combined length, and every number of a note must appear in a list item or paragraph that cites the note's id.
// Pass bare note ids: the first eight characters are what is matched.
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
	secWords := strings.Split(sn, " ")
	windows := make(map[string]bool, len(secWords))
	for i := 0; i+Window <= len(secWords); i++ {
		windows[strings.Join(secWords[i:i+Window], " ")] = true
	}
	units := blocks(section)
	for _, n := range notes {
		words := strings.Split(norm(n.Text), " ")
		found := false
		if len(words) < Window {
			found = strings.Contains(sn, norm(n.Text))
		}
		for s := 0; !found && s+Window <= len(words); s++ {
			found = windows[strings.Join(words[s:s+Window], " ")]
		}
		if !found {
			problems = append(problems, fmt.Sprintf("note %s does not survive in the section (no verbatim run of %d words, or not whole when shorter)", short(n.ID), Window))
		}
		var cited []string
		for _, u := range units {
			if strings.Contains(u, n.ID) || (len(n.ID) >= 8 && strings.Contains(u, n.ID[:8])) {
				cited = append(cited, u)
			}
		}
		if len(cited) == 0 {
			problems = append(problems, fmt.Sprintf("no list item or paragraph cites note %s", short(n.ID)))
			continue
		}
		for _, v := range missingNumbers(n.Text, strings.Join(cited, "\n")) {
			problems = append(problems, fmt.Sprintf("note %s: number %q is missing from the item that cites it", short(n.ID), v))
		}
	}
	return len(problems) == 0, problems
}

// CheckReplace checks new text for an entry against the text it replaces: no heading lost, code fences balanced and
// unchanged in number, length within bounds, and every number of the old text still present.
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
	for _, v := range missingNumbers(original, replacement) {
		problems = append(problems, fmt.Sprintf("number %q is missing from the new text", v))
	}
	return len(problems) == 0, problems
}

// blocks splits a section into list items and paragraphs; a wrapped line belongs to its item, and an indented
// source line ("source [[id]]") belongs to the item above it.
func blocks(section string) []string {
	var out, cur []string
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
		indent := len(l) - len(strings.TrimLeft(l, " \t"))
		subSource := len(cur) > 0 && indent > len(cur[0])-len(strings.TrimLeft(cur[0], " \t")) && len(strings.Fields(body(l))) <= 3
		if bulletLine.MatchString(l) && !subSource {
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
