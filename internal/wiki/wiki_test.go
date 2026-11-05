package wiki

import (
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory-service/internal/md"
)

func TestValidTitle(t *testing.T) {
	good := []string{"Setup", "Embedding model choice", "Őrzés és védelem", "日本語のページ", "Café notes", "v1.2 plan",
		"a", "Wait...", "Index page", "Indexes", strings.Repeat("x", 100), "Q&A (draft)", "100% done", "naïve – dash"}
	for _, g := range good {
		if err := ValidTitle(g); err != nil {
			t.Errorf("ValidTitle(%q) = %v, want ok", g, err)
		}
	}
	bad := map[string]string{
		"":                       "empty",
		" Setup":                 "leading space",
		"Setup ":                 "trailing space",
		".hidden":                "leading dot",
		"a/b":                    "slash",
		`a\b`:                    "backslash",
		"a:b":                    "colon",
		"a|b":                    "pipe",
		"a#b":                    "hash",
		"a^b":                    "caret",
		"a[b":                    "open bracket",
		"a]b":                    "close bracket",
		"Index":                  "reserved",
		"index":                  "reserved, lower case",
		"INDEX":                  "reserved, upper case",
		strings.Repeat("x", 101): "too long",
		strings.Repeat("é", 101): "too long in characters",
		"a*b":                    "star",
		`a"b`:                    "quote",
		"a<b":                    "less-than",
		"a?b":                    "question mark",
		"a\tb":                   "control character",
		"a\nb":                   "newline",
	}
	for b, why := range bad {
		if err := ValidTitle(b); err == nil {
			t.Errorf("ValidTitle(%q) is ok, want an error (%s)", b, why)
		}
	}
	if err := ValidTitle(strings.Repeat("é", 100)); err != nil {
		t.Errorf("100 accented characters are 200 bytes but fit: %v", err)
	}
}

func TestParseMetaReadsTheFiveFields(t *testing.T) {
	src := "---\ntitle: Real Title\nsummary: |\n  One line\n  that wraps.\ntags: [rag, gotcha]\nsources: [2026-10-01, note-a, 42, \"2026-10-02\"]\naliases:\n  - Alt One\n  - Alt Two\n---\n\nBody.\n"
	m := ParseMeta(md.Parse([]byte(src)), "file name")
	if m.Title != "Real Title" || m.Summary != "One line that wraps." {
		t.Fatalf("title/summary: %+v", m)
	}
	eq := func(got, want []string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("got %q want %q", got, want)
		}
	}
	eq(m.Tags, []string{"rag", "gotcha"})
	eq(m.Sources, []string{"2026-10-01", "note-a", "42", "2026-10-02"})
	eq(m.Aliases, []string{"Alt One", "Alt Two"})

	none := ParseMeta(md.Parse([]byte("no frontmatter")), "Plain Name")
	if none.Title != "Plain Name" || none.Summary != "" || len(none.Tags)+len(none.Sources)+len(none.Aliases) != 0 {
		t.Fatalf("defaults: %+v", none)
	}
	one := ParseMeta(md.Parse([]byte("---\naliases: Solo\n---\nx")), "N")
	eq(one.Aliases, []string{"Solo"})
}

func TestFirstSentence(t *testing.T) {
	cases := map[string]string{
		"# Heading\n\nThe widget listens on port 8765. It restarts.\n": "The widget listens on port 8765.",
		"# H\n## H2\n\n```\ncode. here\n```\n\nAfter code. More.\n":    "After code.",
		"\n\n- a bullet point without end\nsecond line.\n":             "a bullet point without end second line.",
		"Version 1.2 is out. Next.":                                    "Version 1.2 is out.",
		"---\n\nText after a rule!\n":                                  "Text after a rule!",
		"# Only a heading\n":                                           "",
		"First paragraph has no end\n\nSecond paragraph. Ok.\n":        "First paragraph has no end",
		"Wrapped\nover two lines. Then more.\n":                        "Wrapped over two lines.",
	}
	for in, want := range cases {
		if got := FirstSentence(in); got != want {
			t.Errorf("FirstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIndexPageIsSortedCappedAndDeterministic(t *testing.T) {
	long := strings.Repeat("word ", 60)
	es := []Entry{{"beta", "Second."}, {"Alpha", "First."}, {"Émile", ""}, {"alpha two", long}, {"Zed", "  spaced \n out  "}}
	a := IndexPage(es)
	rev := []Entry{es[4], es[3], es[2], es[1], es[0]}
	if b := IndexPage(rev); a != b {
		t.Fatalf("order of input changed the page:\n%s\n---\n%s", a, b)
	}
	if !strings.HasPrefix(a, "---\ngenerated: true\ntitle: Index\n---\n\n# Index\n\n") {
		t.Fatalf("head:\n%s", a)
	}
	var names []string
	for _, ln := range strings.Split(a, "\n") {
		if strings.HasPrefix(ln, "- [[") {
			names = append(names, ln[4:strings.Index(ln, "]]")])
			if r := []rune(ln[strings.Index(ln, "]] - ")+5:]); strings.Contains(ln, "]] - ") && len(r) > MaxSummary {
				t.Fatalf("summary over %d characters: %d", MaxSummary, len(r))
			}
		}
	}
	if strings.Join(names, ",") != "Alpha,alpha two,beta,Zed,Émile" {
		t.Fatalf("order: %v", names)
	}
	if !strings.Contains(a, "- [[Émile]]\n") || !strings.Contains(a, "- [[Zed]] - spaced out\n") {
		t.Fatalf("empty summary / spacing:\n%s", a)
	}
	if !strings.Contains(a, "…\n") {
		t.Fatalf("a long summary must end with an ellipsis:\n%s", a)
	}
}
