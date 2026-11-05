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
	"strconv"
	"strings"
	"unicode"
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
	reUnit     = regexp.MustCompile(`\b\d[\d,]*(?:\.\d+)?\s?(?:%|ms|us|µs|ns|s|secs?|mins?|h|hrs?|d|days?|weeks?|bytes?|[kKMGT]i?B|[kKMGT]?bps|[kKMGT]?Hz|x|px|rem|em|k|M|B)\b`)
	rePort     = regexp.MustCompile(`(?i)\bport\s+\d{2,5}\b|\b[a-z][a-z0-9.-]*:\d{2,5}\b`) // "port 80" and "host:8080"; a time such as 09:20 is not a port
	reHost     = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,24}\b`)
	reIdent    = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+\b|\b[a-z]+[A-Z][A-Za-z0-9]*\b`)
	reBacktick = regexp.MustCompile("`([^`\n]{1,300})`")
	reQuoted   = regexp.MustCompile(`(?:^|[\s(\[{:,=*])"([^"\s](?:[^"\n]{0,298}[^"\s])?)"`)                      // an opening quote follows a space, bracket or *; a closing quote follows text
	reSingle   = regexp.MustCompile(`(?:^|[\s(\[{:,=*])'([^'\s](?:[^'\n]{0,198}[^'\s])?)'(?:$|[\s)\]},;:.!?*])`) // 'draft' in SQL or prose; an apostrophe inside a word is not a quote
	reDate     = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2})?)?\b`)
	reNumber   = regexp.MustCompile(`\b\d+(?:[.,]\d+)*\b`) // every number, counts included ("10 notes", "3 attempts")
	reListMark = regexp.MustCompile(`(?m)^\s*\d+[.)]\s`)   // "1. " at the start of a line numbers a list; it is not a value
	reHex      = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)  // commit hashes and short ids; kept only when digits and letters both occur
	reCite     = regexp.MustCompile(`\[\[[^\]]*\]\]`)
	reBullet   = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+[.)])\s+`) // list markers are layout, not words
)

// body is a section without its citations, whose length counts as content nowhere.
func body(s string) string { return reCite.ReplaceAllString(s, "") }

// flat collapses every run of white space to one space, so a rewrapped line still contains the same value.
func flat(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// has reports whether v occurs in text as a whole value: not inside a longer number, name, address or path. "port 80"
// is not found in "port 8080", and an address is not found inside a longer address that starts with it. Both sides are compared with white space
// collapsed, so a value that was rewrapped over a line break is still found.
func has(text, v string) bool { return hasFlat(flat(text), flat(v)) }

// hasFlat is has for text and value whose white space is already collapsed.
func hasFlat(text, v string) bool {
	if v == "" {
		return true
	}
	isWord := func(r byte) bool {
		return r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}
	for from := 0; from <= len(text)-len(v); {
		i := strings.Index(text[from:], v)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(v)
		okBefore := i == 0 || !(isWord(text[i-1]) && isWord(v[0]))
		if v[0] == '/' && i > 0 && (isWord(text[i-1]) || text[i-1] == '/' || text[i-1] == '~' || text[i-1] == '.') {
			okBefore = false // /data/backups is not inside /mnt/data/backups
		}
		okAfter := end == len(text) || !(isWord(text[end]) && isWord(v[len(v)-1]))
		if okAfter && strings.Contains(v, "/") && end+1 < len(text) && text[end] == '/' && (isWord(text[end+1]) || text[end+1] == '.' || text[end+1] == '~' || text[end+1] == '-') {
			okAfter = false // /srv/app is not /srv/app/current
		}
		lastWord := isWord(v[len(v)-1])
		if okAfter && lastWord && end+1 < len(text) && text[end] == '.' && text[end+1] >= '0' && text[end+1] <= '9' {
			okAfter = false // an address followed by a dot and a digit is a longer value
		}
		if okAfter && lastWord && end+1 < len(text) && text[end] == '-' && text[end+1] >= '0' && text[end+1] <= '9' {
			okAfter = false // a hyphen before a digit joins numbers: 80-5 is not 80
		}
		if okBefore && okAfter {
			return true
		}
		from = i + 1
	}
	return false
}

// norm lowercases, drops list markers and turns every character that is not a letter or digit into a space, so that a
// list written as a paragraph (";" between the items) still contains the same run of words.
func norm(s string) string {
	s = strings.ToLower(reBullet.ReplaceAllString(s, ""))
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, s)
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// value is one hard value of a text; Tick marks a value that was written in backticks (code formatting may be dropped
// by a rewrite, so such a value is also accepted as a plain whole word).
type value struct {
	V    string
	Tick bool
}

// quoteFix turns typographic quotes into plain ones, so a rewrite that changes the quote style keeps its values.
var quoteFix = strings.NewReplacer("\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u2018", "'", "\u2019", "'")

func extract(text string) []value {
	seen := map[string]bool{}
	var out []value
	add := func(v string, tick bool) {
		v = strings.TrimLeft(strings.TrimRight(strings.TrimSpace(v), ".,;:!?"), ",;:!?") // a leading dot stays: ../a and ./a differ
		if len(v) < 1 || seen[v] || !strings.ContainsFunc(v, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			return // empty, repeated, or only punctuation ("[]", "{}"): not a value
		}
		seen[v] = true
		out = append(out, value{v, tick})
	}
	text = quoteFix.Replace(text)
	text = strings.ReplaceAll(text, "[[", " ")
	text = stripListMarks(text)
	orig := text
	for _, m := range reURL.FindAllString(text, -1) {
		add(m, false)
	}
	text = reURL.ReplaceAllString(text, " ") // the parts of a URL are not values of their own
	// longest structure first; each match is blanked, so a value is taken once, whole ("~/a/2026-11-03.json" is a
	// path, not also a date and a host)
	for _, re := range []*regexp.Regexp{rePath, reDate, reIP, rePort, reUnit, reVersion, reHost, reIdent} {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			add(m, false)
			return strings.Repeat(" ", len(m))
		})
	}
	for _, m := range reNumber.FindAllString(text, -1) {
		add(m, false)
	}
	// what is left, word by word: anything that holds a digit, an @, a backslash, a colon or an underscore, an option
	// like -x or --force, or a name in capitals, is a value as a whole token ("p95", "gemma4:12b", "D:\\", "-1")
	for _, f := range strings.Fields(reURL.ReplaceAllString(orig, " ")) { // whole tokens of the text as written, so "explicit_only=True" and "48px–96px" are values too
		t := strings.Trim(f, "()[]{}<>\"'`.,;!?*|:")
		if t == "" || !strings.ContainsFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			continue
		}
		switch {
		case strings.ContainsAny(t, "0123456789@\\:_"):
			add(t, false)
		case t[0] == '-' && len(t) > 1:
			add(t, false)
		case len(t) >= 5 && strings.Trim(t, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "":
			add(t, false)
		}
	}
	for _, m := range reHex.FindAllString(orig, -1) {
		if strings.ContainsAny(m, "0123456789") && strings.ContainsAny(m, "abcdef") {
			add(m, false)
		}
	}
	for _, m := range reBacktick.FindAllStringSubmatch(orig, -1) {
		add(m[1], true)
	}
	for _, m := range reQuoted.FindAllStringSubmatch(orig, -1) {
		add(`"`+m[1]+`"`, false) // the quotes are part of the value: "noindex" is not "noindex, nofollow"
	}
	for _, m := range reSingle.FindAllStringSubmatch(orig, -1) {
		add("'"+m[1]+"'", false)
	}
	return out
}

// HardValues lists the values of text that must not be lost, without duplicates, in order of first appearance.
func HardValues(text string) []string {
	var out []string
	for _, v := range extract(text) {
		out = append(out, v.V)
	}
	return out
}

// holder answers "does this text contain the value as a whole value?": the text is read with the same extraction that
// found the value, so the two always agree on where a value starts and ends ("/data/backups" is not a value of a text
// that only has "/mnt/data/backups").
type holder struct {
	flatText string
	set      map[string]bool
}

func newHolder(text string) holder {
	text = quoteFix.Replace(text)
	// list markers are layout: "2." at the start of a line is dropped before the lines are joined, or it would
	// become a value of the new text and hide a changed "2"
	h := holder{flatText: flat(stripListMarks(text)), set: map[string]bool{}}
	// read as written, and with white space collapsed: a value rewrapped over a line break is still one value
	for _, src := range []string{text, h.flatText} {
		for _, v := range extract(src) {
			h.set[v.V] = true
		}
	}
	return h
}

func (h holder) has(v value) bool {
	if h.set[v.V] {
		return true
	}
	return v.Tick && hasFlat(h.flatText, v.V)
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
		for _, l := range units {
			if strings.Contains(l, n.ID) || (len(n.ID) >= 8 && strings.Contains(l, n.ID[:8])) {
				cited = append(cited, l)
			}
		}
		if len(cited) == 0 {
			problems = append(problems, fmt.Sprintf("no list item or paragraph cites note %s", short(n.ID)))
			continue
		}
		for _, v := range missing(n.Text, strings.Join(cited, "\n")) {
			problems = append(problems, fmt.Sprintf("note %s: value %q is missing from the item that cites it", short(n.ID), v))
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
	for _, v := range missing(original, replacement) {
		problems = append(problems, fmt.Sprintf("value %q is missing from the new text", v))
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
		if bulletStart.MatchString(l) && !(len(cur) > 0 && indentOf(l) > indentOf(cur[0]) && len(strings.Fields(body(l))) <= 3) {
			flush() // a more indented bullet that is only a source line ("source [[id]]") belongs to the item above it; one with text of its own is its own item
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

// splitFences separates the text outside code fences from the text inside them.
func splitFences(text string) (prose, code string) {
	var p, c []string
	in := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			in = !in
			continue
		}
		if in {
			c = append(c, l)
		} else {
			p = append(p, l)
		}
	}
	return strings.Join(p, "\n"), strings.Join(c, "\n")
}

// missing lists the values of old that new does not hold. A value written in prose must be in the prose of new (a
// copy of it inside a code fence does not stand for it); a value written in a code fence may be anywhere in new.
func missing(old, new string) []string {
	op, oc := splitFences(old)
	np, _ := splitFences(new)
	whole, prose := newHolder(new), newHolder(np)
	var out []string
	for _, v := range extract(op) {
		if !prose.has(v) {
			out = append(out, v.V)
		}
	}
	for _, v := range extract(oc) {
		if !whole.has(v) {
			out = append(out, v.V)
		}
	}
	return out
}

func indentOf(l string) int {
	return len(l) - len(strings.TrimLeft(l, " \t"))
}

var listLine = regexp.MustCompile(`^(\s*)(\d+)[.)]\s`)

// stripListMarks removes the numbers of a numbered list ("1. ", "2. ", ...). A number at the start of a line is a
// list number only when it is 1, or follows the previous item by one; any other ("7. days" after a wrapped line) is
// part of the text and stays, so changing it is seen.
func stripListMarks(text string) string {
	lines := strings.Split(text, "\n")
	last := 0
	for i, l := range lines {
		if m := listLine.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			if n == 1 || (last > 0 && n == last+1) {
				lines[i] = m[1] + l[len(m[0]):]
				last = n
				continue
			}
			last = 0
			continue
		}
		if strings.TrimSpace(l) != "" && indentOf(l) == 0 {
			last = 0 // plain text ends a list; indented lines belong to the item above
		}
	}
	return strings.Join(lines, "\n")
}
