package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// In token mode /healthz answers only ok: it needs no token, so it must not name projects.
func TestHealthzInTokenModeShowsNoProject(t *testing.T) {
	x := tokenSetup(t)
	resp, err := http.Get(x.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != "ok" {
		t.Errorf("want 200 and ok, got %d %q", resp.StatusCode, b)
	}
}

// A repeated learn with supersedes still replaces the old note, and says so.
func TestDuplicateLearnStillSupersedes(t *testing.T) {
	x := checkedSetup(t)
	wn := x.client(t, "wn")
	old, _ := call(t, wn, "learn", map[string]any{"text": "The old statement to replace."})
	call(t, wn, "learn", map[string]any{"text": "A statement that already exists."})
	out, _ := call(t, wn, "learn", map[string]any{"text": "A statement that already exists.", "supersedes": old[:8]})
	if !strings.Contains(out, "replaced "+old[:8]) {
		t.Errorf("answer does not report the replacement: %q", out)
	}
	if _, still := noteFile(t, x, old); still {
		t.Errorf("the old note is still there")
	}
}

// wiki_read refuses a page that is a symlink into another project's folder.
func TestWikiReadRefusesSymlinkIntoOtherProject(t *testing.T) {
	x := tokenSetup(t)
	if err := os.Symlink("../b/Secret.md", filepath.Join(x.repo, "wiki", "a", "Leak.md")); err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, x.client(t, "ta"), "wiki_read", map[string]any{"path": "a/Leak.md"})
	if strings.Contains(out, "Zebra stripes in b") {
		t.Errorf("read a page of b through a symlink: %q", out)
	}
}
