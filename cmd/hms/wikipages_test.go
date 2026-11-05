package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// conceptOllama is a fake embedding server: every distinct word has an axis of its own, so the cosine of two texts
// is the share of words they have in common.
func conceptOllama(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	axis := map[string]int{}
	re := regexp.MustCompile(`[\p{L}\p{N}_]+`)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"nomic-embed-text:latest"}]}`)
		case "/api/embed":
			var in struct{ Input []string }
			json.NewDecoder(r.Body).Decode(&in)
			mu.Lock()
			defer mu.Unlock()
			out := make([][]float32, len(in.Input))
			for i, text := range in.Input {
				v := make([]float32, 512)
				var n float32
				for _, wd := range re.FindAllString(strings.ToLower(text), -1) {
					a, ok := axis[wd]
					if !ok {
						a = len(axis)
						axis[wd] = a
					}
					v[a]++
				}
				for _, x := range v {
					n += x * x
				}
				if n == 0 {
					v[511], n = 1, 1
				}
				for j := range v {
					v[j] /= sqrt32(n)
				}
				out[i] = v
			}
			json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func sqrt32(x float32) float32 {
	z := x
	for i := 0; i < 30; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// words makes a page text: the shared core words of a cluster plus words of the page's own.
func words(core string, own string, nOwn int) string {
	var b []string
	for i := 0; i < 10; i++ {
		b = append(b, fmt.Sprintf("%score%d", core, i))
	}
	for i := 0; i < nOwn; i++ {
		b = append(b, fmt.Sprintf("%sown%d", own, i))
	}
	return strings.Join(b, " ") + "\n"
}

// pairsWiki builds two clusters of six pages in two projects. In cluster x: a, a (copy) (a with one word changed),
// b, f1, f2, f3; in cluster y: c, g1 .. g5. Inside a cluster the pages share half of their words.
func pairsWiki(t *testing.T, url string) (home, pairs string) {
	home = t.TempDir()
	if _, _, c := hms(t, home, url, nil, "", "init"); c != 0 {
		t.Fatal("init failed")
	}
	put := func(rel, content string) {
		p := filepath.Join(home, "memory", "wiki", filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := words("x", "a", 10)
	put("x/a.md", "---\nsummary: The first page\n---\n"+a)
	put("x/a (copy).md", strings.Replace(a, "aown9", "changed", 1))
	for _, n := range []string{"b", "f1", "f2", "f3"} {
		put("x/"+n+".md", words("x", n, 10))
	}
	put("y/c.md", words("y", "c", 10))
	for _, n := range []string{"g1", "g2", "g3", "g4", "g5"} {
		put("y/"+n+".md", words("y", n, 10))
	}
	pairs = filepath.Join(t.TempDir(), "pairs.json")
	os.WriteFile(pairs, []byte(`{"version":1,
 "related":[["x/a.md","x/b.md"],["x/a.md","y/c.md"]],
 "unrelated":[["x/a.md","y/c.md"],["x/a.md","x/b.md"]],
 "near_duplicates":{"sources":["x/a.md","y/c.md"]}}`), 0o644)
	return home, pairs
}

func TestWikiRelatedDuplicatesAndEvalPairs(t *testing.T) {
	up := conceptOllama(t)
	home, pairs := pairsWiki(t, up.URL)

	if out, _, c := hms(t, home, up.URL, nil, "", "wiki", "related", "a"); c != 1 || !strings.Contains(out, "no vectors yet for [[x/a]]") {
		t.Fatalf("before embed: %d %q", c, out)
	}
	if out, _, c := hms(t, home, up.URL, nil, "", "wiki", "duplicates"); c != 0 || !strings.Contains(out, "duplicates: 0 (cosine >= 0.92; 0 pages compared, 12 with no vectors yet)") {
		t.Fatalf("duplicates before embed: %d %q", c, out)
	}
	if _, e, c := hms(t, home, up.URL, nil, "", "embed"); c != 0 {
		t.Fatalf("embed: %s", e)
	}

	out, _, c := hms(t, home, up.URL, nil, "", "wiki", "related", "a", "-n", "3")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if c != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "0.95  [[a (copy)]]") || !strings.HasPrefix(lines[1], "0.52  [[b]]") {
		t.Fatalf("related: %d\n%s", c, out)
	}
	if out, _, _ := hms(t, home, up.URL, nil, "", "wiki", "related", "-p", "x", "x/b.md", "-n", "1"); !strings.HasPrefix(out, "0.52  [[a]]  The first page\n") && !strings.HasPrefix(out, "0.52  [[a (copy)]]") {
		t.Fatalf("related with a path and a summary: %q", out)
	}
	if _, e, c := hms(t, home, up.URL, nil, "", "wiki", "related", "Nowhere"); c != 1 || !strings.Contains(e, "no wiki entry") {
		t.Fatalf("unknown page: %d %q", c, e)
	}

	out, _, c = hms(t, home, up.URL, nil, "", "wiki", "duplicates")
	if c != 0 || out != "0.95  [[x/a]]  [[x/a (copy)]]\nduplicates: 1 (cosine >= 0.92; 12 pages compared, 0 with no vectors yet)\n" {
		t.Fatalf("duplicates: %d %q", c, out)
	}
	if out, _, _ := hms(t, home, up.URL, nil, "", "wiki", "duplicates", "-p", "y", "-t", "0.4"); strings.Count(out, "[[c]]") != 5 || !strings.Contains(out, "duplicates: 15 (cosine >= 0.40; 6 pages compared") {
		t.Fatalf("duplicates in y at 0.4 (six pages share half of their words: 15 pairs): %q", out)
	}

	out, _, c = hms(t, home, up.URL, nil, "", "wiki", "eval-pairs", "-pairs", pairs)
	if c != 0 {
		t.Fatalf("eval-pairs must exit 0, got %d", c)
	}
	for _, want := range []string{
		"related    PASS    0.524", "x/a.md <-> x/b.md",
		"related    FAIL    0.048", "x/a.md <-> y/c.md",
		"unrelated  PASS    0.048",
		"unrelated  FAIL    0.524",
		"duplicate  PASS    0.952", "x/a.md <-> x/a (copy).md",
		"duplicate  FAIL    no vectors yet", "y/c.md <-> y/c (copy).md",
		"related pairs passing: 1/2", "unrelated pairs passing: 1/2", "duplicate copies found: 1/2",
		"smallest related cosine: 0.048 (x/a.md <-> y/c.md)",
		"largest unrelated cosine: 0.524 (x/a.md <-> x/b.md)",
		"non-copy pairs flagged as duplicates at 0.92: 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("eval-pairs lacks %q:\n%s", want, out)
		}
	}
	if _, e, c := hms(t, home, up.URL, nil, "", "wiki", "eval-pairs"); c != 2 || !strings.Contains(e, "usage") {
		t.Fatalf("no pairs file: %d %q", c, e)
	}
}
