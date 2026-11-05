package md

import (
	"strings"
	"testing"
)

func TestSetKeysDropsTheLinesOfABlockListItReplaces(t *testing.T) {
	for name, src := range map[string]string{
		"column 0":  "---\ntitle: P\ntags:\n- net\n- ops\nsummary: s\n---\nbody\n",
		"indented":  "---\ntitle: P\ntags:\n  - net\n  - ops\nsummary: s\n---\nbody\n",
		"comment":   "---\ntitle: P\ntags: # labels\n- net\n- ops\nsummary: s\n---\nbody\n",
		"crlf col0": "---\r\ntitle: P\r\ntags:\r\n- net\r\n- ops\r\nsummary: s\r\n---\r\nbody\r\n",
		"crlf ind":  "---\r\ntitle: P\r\ntags:\r\n  - net\r\nsummary: s\r\n---\r\nbody\r\n",
	} {
		out, err := SetKeys([]byte(src), map[string]any{"tags": []string{"net", "ops", "new"}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d := Parse(out)
		if d.Error != "" {
			t.Fatalf("%s: frontmatter broken: %s\n%s", name, d.Error, out)
		}
		if got := d.List("tags"); strings.Join(got, ",") != "net,ops,new" {
			t.Errorf("%s: tags %v\n%s", name, got, out)
		}
		if d.Str("summary") != "s" || d.Str("title") != "P" || !strings.HasSuffix(string(out), "body\n") && !strings.HasSuffix(string(out), "body\r\n") {
			t.Errorf("%s: other keys changed:\n%s", name, out)
		}
	}
}

func TestSetKeysFoldedSummaryAndAFollowingBlockList(t *testing.T) {
	src := "---\ntitle: P\nsummary: >\n  folded\n  text\ntags:\n- a\nsources:\n- 2026-10-01: abc\n---\nbody\n"
	out, err := SetKeys([]byte(src), map[string]any{"summary": "new", "tags": []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	d := Parse(out)
	if d.Error != "" || d.Str("summary") != "new" || strings.Join(d.List("tags"), ",") != "a,b" {
		t.Fatalf("%v\n%s", d.Error, out)
	}
	if !strings.Contains(string(out), "sources:\n- 2026-10-01: abc\n") {
		t.Errorf("the untouched block list was changed:\n%s", out)
	}
}

func TestSetKeysAColumnZeroDashAfterAScalarKeyIsNotDropped(t *testing.T) {
	// "- x" at column 0 after a key that has a value belongs to nobody in valid YAML; only an empty value owns its list
	out, err := SetKeys([]byte("---\ntitle: P\ntags:\n- x\n---\nb\n"), map[string]any{"title": "Q"})
	if err != nil || !strings.Contains(string(out), "tags:\n- x\n") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestWikiLinksIgnoreCodeAndEmbeds(t *testing.T) {
	text := "See [[Real]] and `[[inline]]` and ``a [[double]] b``.\n" +
		"```bash\nif [[ -f /etc/w.conf ]]; then echo ok; fi\n[[InFence]]\n```\n" +
		"~~~\n[[InTilde]]\n~~~\n" +
		"![[diagram.png]] and ![[Embedded Note]] and [[Other|alias]] [[Third#sec]]\n" +
		"````\n```\n[[InLongFence]]\n```\n````\n[[After]]\n"
	got := strings.Join(WikiLinks(text), ",")
	if want := "Real,Other,Third,After"; got != want {
		t.Fatalf("links %q, want %q", got, want)
	}
	// an unclosed fence runs to the end of the text
	if got := WikiLinks("[[A]]\n```\n[[B]]\n"); len(got) != 1 || got[0] != "A" {
		t.Fatalf("%v", got)
	}
	// CRLF
	if got := WikiLinks("[[A]]\r\n```\r\n[[B]]\r\n```\r\n[[C]]\r\n"); strings.Join(got, ",") != "A,C" {
		t.Fatalf("%v", got)
	}
}

func TestFenceTracksBothMarkers(t *testing.T) {
	var f Fence
	var in []bool
	for _, l := range []string{"a", "~~~", "```", "~~~", "b", "```", "c"} {
		in = append(in, f.Line(l))
	}
	want := []bool{false, true, true, true, false, true, true}
	// "```" inside a ~~~ block does not close it; after ~~~ closes, "```" opens a block that "c" is still inside
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("line %d: %v, want %v (%v)", i, in[i], want[i], in)
		}
	}
	if !f.Open() {
		t.Fatal("the last fence is open")
	}
}
