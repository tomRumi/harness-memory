package md

import (
	"strings"
	"testing"
)

func TestSetKeysChangesOnlyWhatItMust(t *testing.T) {
	src := "---\n# a comment\nid: 00123\nstatus: active\nzeta: 'single quoted'\nkind: rag:gotcha\n---\n\nBody line one.\r\nBody line two.\n"
	out, err := SetKeys([]byte(src), map[string]any{"status": "retired", "retired_reason": "wrong: really"})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n# a comment\nid: 00123\nstatus: retired\nzeta: 'single quoted'\nkind: rag:gotcha\nretired_reason: \"wrong: really\"\n---\n\nBody line one.\r\nBody line two.\n"
	if string(out) != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
	d := Parse(out)
	if d.Error != "" || d.Str("status") != "retired" || d.Str("retired_reason") != "wrong: really" {
		t.Fatalf("result does not parse back: %+v", d)
	}
}

func TestSetKeysNoFrontmatterAndRefusals(t *testing.T) {
	out, err := SetKeys([]byte("# Title\n\ntext\n"), map[string]any{"status": "archived"})
	if err != nil || !strings.HasPrefix(string(out), "---\nstatus: archived\n---\n# Title") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := SetKeys([]byte("---\ntitle: [unclosed\n---\nbody\n"), map[string]any{"status": "x"}); err == nil {
		t.Fatal("a malformed frontmatter block must not be rewritten")
	}
	if _, err := SetKeys([]byte("---\ntitle: x\nno closing\n"), map[string]any{"status": "x"}); err == nil {
		t.Fatal("an unclosed block must not be rewritten")
	}
	// windows line endings stay
	out, _ = SetKeys([]byte("---\r\nstatus: active\r\n---\r\nbody\r\n"), map[string]any{"status": "retired", "extra": true})
	if !strings.Contains(string(out), "status: retired\r\n") || !strings.Contains(string(out), "extra: true\r\n") || !strings.HasSuffix(string(out), "body\r\n") {
		t.Fatalf("%q", out)
	}
	// numbers-as-strings stay strings
	out, _ = SetKeys([]byte("---\nstatus: active\n---\nx\n"), map[string]any{"superseded_by": "00123", "k": "true"})
	if !strings.Contains(string(out), `superseded_by: "00123"`) || !strings.Contains(string(out), `k: "true"`) {
		t.Fatalf("%q", out)
	}
}

func TestSetKeysMultiLineValuesAndQuotedKeys(t *testing.T) {
	src := "---\nid: 7\nstatus: >-\n  active\n  really\ntitle: x\n---\nbody\n"
	out, err := SetKeys([]byte(src), map[string]any{"status": "retired"})
	if err != nil || string(out) != "---\nid: 7\nstatus: retired\ntitle: x\n---\nbody\n" {
		t.Fatalf("%q %v", out, err)
	}
	if d := Parse(out); d.Error != "" || d.Str("status") != "retired" {
		t.Fatalf("%+v", d)
	}
	out, err = SetKeys([]byte("---\n\"status\": active\nid: 1\n---\nb\n"), map[string]any{"status": "retired"})
	if err != nil {
		t.Fatal(err)
	}
	if d := Parse(out); d.Error != "" || d.Str("status") != "retired" || strings.Count(string(out), "status") != 1 {
		t.Fatalf("%q %+v", out, d)
	}
}

func TestSetKeysWritesAListAsAFlowList(t *testing.T) {
	src := "---\ntitle: T\ntags: [old]\nnote: keep # me\n---\n\nBody.\n"
	out, err := SetKeys([]byte(src), map[string]any{"tags": []string{"old", "new tag", "ü"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "---\ntitle: T\ntags: [\"old\",\"new tag\",\"ü\"]\nnote: keep # me\n---\n\nBody.\n"; string(out) != want {
		t.Fatalf("got %q", out)
	}
	if got := Parse(out).List("tags"); len(got) != 3 || got[1] != "new tag" || got[2] != "ü" {
		t.Fatalf("parsed back %v", got)
	}
}
