package curate

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

// Window is the number of consecutive words of a note that must survive verbatim in the page.
const Window = 8

// Note is one waiting note: its bare id (the file name without .md) and its text.
type Note struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

var (
	spaces     = regexp.MustCompile(`\s+`)
	reNumber   = regexp.MustCompile(`\d+(?:[.,]\d+)*`)
	reListMark = regexp.MustCompile(`(?m)^\s*\d+[.)]\s`)        // "1. " at the start of a line numbers a list; it is not a value
	reEnum     = regexp.MustCompile(`(^|\s)\(?\d{1,2}\)(\s|$)`) // "(1) first ... (2) second" inside a sentence numbers items; it is not a value
	reHeading  = regexp.MustCompile(`^#{1,6}\s`)
)

func flat(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// norm folds a text for comparison: Unicode NFC, lower case, and only letters, digits, marks and underscore are kept.
// Everything else (punctuation, list markers, curly quotes and straight quotes, en and em dashes and hyphens) is a
// separator, so a note with an apostrophe or a dash is found in a page that spells it the other way. A text with no
// letter or digit gives "".
func normText(s string) string {
	s = strings.ToLower(norm.NFC.String(reListMark.ReplaceAllString(s, "")))
	var b strings.Builder
	for _, r := range s {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return flat(b.String())
}

// numbers lists the numbers of a note text, whole ("80" is not "8080"), without list numbers, each once.
func numbers(text string) []string {
	seen := map[string]bool{}
	var out []string
	text = reEnum.ReplaceAllString(reEnum.ReplaceAllString(text, "$1 $2"), "$1 $2") // twice: neighbours share a space
	for _, n := range reNumber.FindAllString(reListMark.ReplaceAllString(text, ""), -1) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// nonBlank returns the lines of s that are not blank, with white space collapsed and the ends trimmed.
func nonBlank(s string) []string {
	var out []string
	for _, l := range strings.Split(md.NormalizeNL(s), "\n") {
		if l = flat(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// section is the text between two headings; the heading line belongs to the section it opens. Headings inside a
// code fence do not count.
func sections(body string) []string {
	var out []string
	var cur []string
	var f md.Fence
	for _, l := range strings.Split(md.NormalizeNL(body), "\n") {
		if !f.Line(l) && reHeading.MatchString(strings.TrimSpace(l)) && len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
			cur = nil
		}
		cur = append(cur, l)
	}
	return append(out, strings.Join(cur, "\n"))
}

// CheckPage checks the new body of a page against the old body (empty for a new page) and the notes assigned to it.
// It returns the problems found; none means the page passes. resolve says whether a [[link]] leads to a page.
//
// It counts and compares; it does not judge meaning. The rules, in order:
//
//	insert only   every non-blank line of the old body is in the new body, in the same order
//	each note     an 8-word run of the note (a note under 8 words: the whole note) is in the new body
//	numbers       every number of a note is in the section of the new body where its run was found
//	links         every [[link]] resolves
//	fences        the merge adds an even number of ``` or ~~~ lines
//	length        not shorter than the old body, not longer than old + 3 x the notes + 2,000 characters
func CheckPage(oldBody, newBody string, notes []Note, resolve func(link string) bool) []string {
	var probs []string

	// (i) insert only: a greedy subsequence match of the old lines in the new lines
	oldLines, newLines := nonBlank(oldBody), nonBlank(newBody)
	j := 0
	for _, ol := range oldLines {
		for j < len(newLines) && newLines[j] != ol {
			j++
		}
		if j == len(newLines) {
			probs = append(probs, fmt.Sprintf("insert only: the old line %q is gone or out of order", wiki.Clip(ol, 60)))
			break
		}
		j++
	}

	// (ii) and (iii): window sets are built once per section, so a large page costs one pass
	secs := sections(newBody)
	secNorm := make([]string, len(secs))
	windows := map[string][]int{} // 8-word run -> sections that hold it
	for i, s := range secs {
		secNorm[i] = normText(s)
		w := strings.Split(secNorm[i], " ")
		for k := 0; k+Window <= len(w); k++ {
			key := strings.Join(w[k:k+Window], " ")
			if l := windows[key]; len(l) == 0 || l[len(l)-1] != i {
				windows[key] = append(l, i)
			}
		}
	}
	for _, n := range notes {
		nn := normText(n.Text)
		words := strings.Split(nn, " ")
		found := map[int]bool{} // sections where a run of the note was found
		if nn == "" {
			// a note with no letter or digit has nothing to find: it is never in the page
		} else if len(words) < Window {
			for i, s := range secNorm {
				if strings.Contains(" "+s+" ", " "+nn+" ") {
					found[i] = true
				}
			}
		} else {
			for k := 0; k+Window <= len(words); k++ {
				for _, i := range windows[strings.Join(words[k:k+Window], " ")] {
					found[i] = true
				}
			}
		}
		if len(found) == 0 {
			probs = append(probs, fmt.Sprintf("note %s is not in the page (no run of %d words, or not whole when shorter)", short(n.ID), Window))
			continue
		}
		have := map[string]bool{}
		for i := range found {
			for _, x := range reNumber.FindAllString(secs[i], -1) {
				have[x] = true
			}
		}
		for _, x := range numbers(n.Text) {
			if !have[x] {
				probs = append(probs, fmt.Sprintf("note %s: the number %s is not in the section where the note was placed", short(n.ID), x))
			}
		}
	}

	// (iv) links. Only links the merge adds are judged; a link that was already in the old text (even a broken one)
	// is old text, and old text is kept as it is.
	oldLinks := map[string]bool{}
	for _, l := range md.WikiLinks(oldBody) {
		oldLinks[l] = true
	}
	for _, l := range md.WikiLinks(newBody) {
		if !oldLinks[l] && !resolve(l) {
			probs = append(probs, fmt.Sprintf("broken link [[%s]]", wiki.Clip(l, 60)))
		}
	}

	// (v) fences: the merge must not leave a fence open. A fence closes only with the same character and at least as
	// many of them as it opened with. A page that already ended inside a fence (a hand edit) is old text and is kept
	// as it is.
	if endsInFence(newBody) && !endsInFence(oldBody) {
		probs = append(probs, "the merge leaves a ``` or ~~~ block open at the end of the page")
	}

	// (vi) length, counted on the lines that matter (blank lines and spacing do not count)
	oldLen, newLen := utf8.RuneCountInString(strings.Join(oldLines, "\n")), utf8.RuneCountInString(strings.Join(newLines, "\n"))
	total := 0
	for _, n := range notes {
		total += utf8.RuneCountInString(n.Text)
	}
	if newLen < oldLen {
		probs = append(probs, fmt.Sprintf("the page is shorter than before (%d < %d characters)", newLen, oldLen))
	}
	if max := oldLen + 3*total + 2000; newLen > max {
		probs = append(probs, fmt.Sprintf("the page is too long: %d characters, at most %d for these notes", newLen, max))
	}
	return probs
}

// endsInFence reports whether a fenced code block is still open at the end of body.
func endsInFence(body string) bool {
	var f md.Fence
	for _, l := range strings.Split(md.NormalizeNL(body), "\n") {
		f.Line(l)
	}
	return f.Open()
}
