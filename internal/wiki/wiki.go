// Package wiki holds the rules of a wiki page that do not need the index: what a page title may be, which
// frontmatter fields a page has, and how the generated Index page is written.
package wiki

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/tomRumi/harness-memory-service/internal/md"
)

// IndexTitle is the title and file name (without .md) of the generated index page of a project. It is reserved:
// no curated page may take it.
const IndexTitle = "Index"

// UnsortedTitle is the page the service itself writes the notes of a failed merge to.
const UnsortedTitle = "Unsorted"

// MaxTitle is the longest title, in characters.
const MaxTitle = 100

// MaxTitleBytes is the longest title, in bytes of UTF-8 (file names are limited in bytes, not characters).
const MaxTitleBytes = 200

// Fold is the form in which titles and link targets are compared: Unicode NFC, then full Unicode case folding. The
// file system of a Mac and Obsidian treat "Café" written in one piece or two, "Ports" and "ports", "Strasse" and
// "Straße", "ΟΔΟΣ" and "οδος", "file" and "ﬁle" as one name. Titles are compared with this function everywhere.
func Fold(s string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(s))) }

// badChars cannot be in a title: they break links ([[A|B]], [[A#B]], [[A^B]]), paths, or are rejected as file
// names by Obsidian or by common file systems.
const badChars = "/\\:|#^[]*\"<>?"

// ValidTitle says whether title can be the name of a new curated wiki page. The title is the file name, so every
// rule here is a rule of a file name that Obsidian can open and that a [[link]] can name.
func ValidTitle(title string) error {
	switch {
	case title == "":
		return fmt.Errorf("a page title cannot be empty")
	case len([]rune(title)) > MaxTitle:
		return fmt.Errorf("page title %q is longer than %d characters", Clip(title, 30), MaxTitle)
	case len(title) > MaxTitleBytes:
		return fmt.Errorf("page title %q is longer than %d bytes", Clip(title, 30), MaxTitleBytes)
	case title != strings.TrimSpace(title):
		return fmt.Errorf("page title %q has a space at the start or the end", title)
	case strings.HasPrefix(title, "."):
		return fmt.Errorf("page title %q starts with a dot", title)
	case Fold(title) == Fold(IndexTitle):
		return fmt.Errorf("page title %q is reserved for the generated index page", title)
	}
	for _, r := range title {
		if strings.ContainsRune(badChars, r) {
			return fmt.Errorf("page title %q contains %q, which a file name or a link cannot hold", title, r)
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("page title %q contains a control character", title)
		}
	}
	return nil
}

// ValidModelTitle is ValidTitle for a page a model writes: the name of the service's own Unsorted page is reserved too.
func ValidModelTitle(title string) error {
	if err := ValidTitle(title); err != nil {
		return err
	}
	if Fold(title) == Fold(UnsortedTitle) {
		return fmt.Errorf("the title %q belongs to the service", UnsortedTitle)
	}
	return nil
}

// IsSpecial says whether title is the Index page or the Unsorted page: pages that are lists, not subjects.
func IsSpecial(title string) bool {
	f := Fold(title)
	return f == Fold(IndexTitle) || f == Fold(UnsortedTitle)
}

// Meta is what a page says about itself in its frontmatter.
type Meta struct {
	Title   string   // frontmatter title, or the file name when there is none
	Summary string   // one line
	Tags    []string // free labels
	Sources []string // ids or dates of the notes merged into the page
	Aliases []string // other names a [[link]] may use
}

// ParseMeta reads the fields of a page. fileName is the file name without .md.
func ParseMeta(d md.Doc, fileName string) Meta {
	m := Meta{Title: d.Str("title"), Summary: oneLine(d.Str("summary"))}
	if m.Title == "" {
		m.Title = fileName
	}
	m.Tags, m.Sources, m.Aliases = list(d, "tags"), list(d, "sources"), list(d, "aliases")
	return m
}

// list reads a list of strings. A bare YAML date or number (sources: [2026-10-01]) is a value, not a string, to
// the parser; it is read back in its usual written form.
func list(d md.Doc, key string) []string {
	var raw []any
	switch v := d.FM[key].(type) {
	case nil:
		return nil
	case []any:
		raw = v
	default:
		raw = []any{v}
	}
	var out []string
	for _, x := range raw {
		var s string
		switch v := x.(type) {
		case string:
			s = v
		case time.Time:
			s = v.Format("2006-01-02")
			if v.Hour() != 0 || v.Minute() != 0 || v.Second() != 0 {
				s = v.Format(time.RFC3339)
			}
		case nil:
		case map[string]any: // "- 2026-10-01: abc" is a one-entry map to YAML; show it as the text it was
			s = mapText(v)
		case map[any]any:
			m := map[string]any{}
			for k, val := range v {
				m[scalarText(k)] = val
			}
			s = mapText(m)
		default:
			s = fmt.Sprint(v)
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func scalarText(x any) string {
	if t, ok := x.(time.Time); ok {
		return t.Format("2006-01-02")
	}
	return fmt.Sprint(x)
}

// mapText writes a map as "key: value" (entries sorted by key, joined by ", ").
func mapText(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + scalarText(m[k])
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// FirstSentence returns the first sentence of the first paragraph of a page body, skipping headings, code fences,
// rules and blank lines. Empty when the body has no text.
func FirstSentence(body string) string {
	var parts []string
	var fence md.Fence
	for _, ln := range strings.Split(md.NormalizeNL(body), "\n") {
		t := strings.TrimSpace(ln)
		skip := fence.Line(ln) || strings.HasPrefix(t, "#") || strings.Trim(t, "-*_ ") == "" && t != ""
		if t == "" || skip {
			if len(parts) > 0 {
				break
			}
			continue
		}
		for _, p := range []string{"- ", "* ", "+ ", "> "} {
			t = strings.TrimPrefix(t, p)
		}
		parts = append(parts, t)
		if end := sentenceEnd(strings.Join(parts, " ")); end >= 0 {
			return strings.TrimSpace(strings.Join(parts, " ")[:end])
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// sentenceEnd is the index just after the first ". ", "! " or "? " (or the end punctuation of the text), or -1
// when the sentence is not finished yet.
func sentenceEnd(s string) int {
	r := []rune(s)
	for i, c := range r {
		if (c == '.' || c == '!' || c == '?') && (i+1 == len(r) || r[i+1] == ' ') {
			return len(string(r[:i+1]))
		}
	}
	return -1
}

// MaxSummary is the longest summary shown in the index, in characters.
const MaxSummary = 140

// Entry is one line of the index page.
type Entry struct {
	Name    string // the file name without .md; the link target
	Summary string // the page's summary, or its first sentence
}

// IndexPage returns the markdown of wiki/<project>/Index.md for the given pages (already without the Index page).
// The order is the title order, case-insensitive; the same pages always give the same text.
func IndexPage(entries []Entry) string {
	es := append([]Entry(nil), entries...)
	sortEntries(es)
	var b strings.Builder
	for _, e := range es {
		b.WriteString("- [[" + e.Name + "]]")
		if s := Cap(oneLine(e.Summary), MaxSummary); s != "" {
			b.WriteString(" - " + s)
		}
		b.WriteString("\n")
	}
	body := "# " + IndexTitle + "\n\n" + b.String()
	return string(md.Render(map[string]any{"title": IndexTitle, "generated": true}, body))
}

// Cap shortens s to at most n characters, ending with an ellipsis when it was cut.
func Cap(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := Fold(es[i].Name), Fold(es[j].Name)
		if a != b {
			return a < b
		}
		return es[i].Name < es[j].Name
	})
}
