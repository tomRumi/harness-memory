package md

import (
	"reflect"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	d := Parse([]byte("---\ntitle: Alpha\nstatus: retired\npinned: true\nlinks_to: [one, two]\n---\n\nBody [[Target|alias]] and [[Other#sec]].\n"))
	if d.Error != "" || d.Str("title") != "Alpha" || d.Str("status") != "retired" || !d.Bool("pinned") {
		t.Fatalf("bad parse: %+v", d)
	}
	if got := d.List("links_to"); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("list: %v", got)
	}
	if got := WikiLinks(d.Body); !reflect.DeepEqual(got, []string{"Target", "Other"}) {
		t.Fatalf("wikilinks: %v", got)
	}
}

func TestParseNoFrontmatterAndMalformed(t *testing.T) {
	if d := Parse([]byte("# Just text\n")); d.FM != nil || d.Error != "" || d.Body != "# Just text\n" {
		t.Fatalf("plain: %+v", d)
	}
	d := Parse([]byte("---\ntitle: [unclosed\n---\nbody\n"))
	if d.Error == "" || d.Body == "" {
		t.Fatalf("malformed frontmatter must keep the file readable and report: %+v", d)
	}
	if d := Parse([]byte("---\ntitle: x\nno closing")); d.Error == "" {
		t.Fatal("unclosed block must report an error")
	}
}

func TestRenderIsDeterministicAndRoundTrips(t *testing.T) {
	fm := map[string]any{"b": "2", "a": "1", "status": "active"}
	a, b := Render(fm, "text"), Render(fm, "text")
	if string(a) != string(b) {
		t.Fatal("render differs between calls")
	}
	d := Parse(a)
	if d.Str("a") != "1" || d.Str("b") != "2" || d.Body != "text\n" {
		t.Fatalf("round trip: %+v", d)
	}
}
