package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

// wikiFixture builds a small wiki under a throwaway home: a page with spaces and accents in its title, one broken
// link and one ambiguous link.
func wikiFixture(t *testing.T, url string) string {
	home := t.TempDir()
	if _, _, c := hms(t, home, url, nil, "", "init"); c != 0 {
		t.Fatal("init failed")
	}
	repo := filepath.Join(home, "memory")
	put := func(rel, content string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("wiki/proj/Őrzés és café.md", "---\nsummary: A page with spaces and accents\ntags: [demo]\n---\n\nSee [[Plain]], [[Nowhere]] and [[Twin]].\n")
	put("wiki/proj/Plain.md", "Plain explains the basics. It links back to [[őrzés ÉS café|the first page]].\n")
	put("wiki/other/Twin.md", "one twin\n")
	put("wiki/third/Twin.md", "second twin\n")
	return home
}

func TestWikiLinksIndexAndDoctorTranscript(t *testing.T) {
	up := fakeOllama(t)
	home := wikiFixture(t, up.URL)
	repo := filepath.Join(home, "memory")

	out, errOut, c := hms(t, home, up.URL, nil, "", "wiki", "links")
	want := "wiki/proj/Őrzés és café.md: broken link [[Nowhere]]\n" +
		"wiki/proj/Őrzés és café.md: ambiguous link [[Twin]] (could be wiki/other/Twin.md, wiki/third/Twin.md)\n" +
		"links: 1 broken, 1 ambiguous\n"
	if c != 0 || out != want {
		t.Fatalf("exit %d\n%s\nwant\n%s\nstderr: %s", c, out, want, errOut)
	}
	if out, _, c = hms(t, home, up.URL, nil, "", "wiki", "links", "-p", "other"); c != 0 || out != "links: 0 broken, 0 ambiguous\n" {
		t.Fatalf("project filter: %d %q", c, out)
	}
	dr, _, c := hms(t, home, up.URL, nil, "", "doctor")
	if c != 0 || !strings.Contains(dr, "WARN  links") || !strings.Contains(dr, "1 broken, 1 ambiguous") {
		t.Fatalf("doctor: %d\n%s", c, dr)
	}

	out, errOut, c = hms(t, home, up.URL, nil, "", "wiki", "index")
	if c != 0 || out != "wrote wiki/other/Index.md\nwrote wiki/proj/Index.md\nwrote wiki/third/Index.md\n" {
		t.Fatalf("index: %d %q %s", c, out, errOut)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "wiki/proj/Index.md"))
	if want := "---\ngenerated: true\ntitle: Index\n---\n\n# Index\n\n- [[Plain]] - Plain explains the basics.\n- [[Őrzés és café]] - A page with spaces and accents\n"; string(b) != want {
		t.Fatalf("Index.md:\n%s", b)
	}
	if out, _, c = hms(t, home, up.URL, nil, "", "wiki", "index"); c != 0 || out != "unchanged wiki/other/Index.md\nunchanged wiki/proj/Index.md\nunchanged wiki/third/Index.md\n" {
		t.Fatalf("second index: %d %q", c, out)
	}
	if out, _, c = hms(t, home, up.URL, nil, "", "wiki", "index", "-p", "proj"); c != 0 || out != "unchanged wiki/proj/Index.md\n" {
		t.Fatalf("one project: %d %q", c, out)
	}
	log, err := exec.Command("git", "-C", repo, "-c", "core.quotepath=off", "log", "--name-only", "--format=%s").Output()
	if err != nil || !strings.Contains(string(log), "wiki/proj/Index.md") {
		t.Fatalf("the index page was not committed: %v\n%s", err, log)
	}
	if n, _ := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output(); strings.TrimSpace(string(n)) != "1" {
		t.Fatalf("one commit expected for the first index run, got %s", n)
	}
	// the index pages are all committed; the hand-written pages are not the index command's to commit
	if st, _ := exec.Command("git", "-C", repo, "status", "--porcelain", "--", "wiki/proj/Index.md", "wiki/other/Index.md", "wiki/third/Index.md").Output(); len(st) != 0 {
		t.Fatalf("status: %s", st)
	}
	if _, _, c = hms(t, home, up.URL, nil, "", "wiki", "bogus"); c != 2 {
		t.Fatalf("unknown subcommand must be a usage error: %d", c)
	}
}

// TestGeneratedWikiIsAnObsidianVault checks the rules that make the folder open cleanly in Obsidian. Obsidian itself
// cannot run in the headless test environment; this test does not claim that it was opened there.
func TestGeneratedWikiIsAnObsidianVault(t *testing.T) {
	up := fakeOllama(t)
	home := wikiFixture(t, up.URL)
	if _, _, c := hms(t, home, up.URL, nil, "", "wiki", "index"); c != 0 {
		t.Fatal("index failed")
	}
	root := filepath.Join(home, "memory", "wiki")
	rejected := regexp.MustCompile("[\\x00-\\x1f\\x7f*\"<>?:|\\\\]")
	mdLink := regexp.MustCompile(`\]\([^)]*\.md[^)]*\)`)
	names := map[string]bool{}
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if seg == "." {
				continue
			}
			if rejected.MatchString(seg) || strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, ".") || strings.HasPrefix(seg, ".") {
				t.Errorf("%s: a name Obsidian rejects or hides", rel)
			}
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) < 6 {
		t.Fatalf("fixture too small: %v", files)
	}
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		if !strings.HasSuffix(p, ".md") {
			t.Errorf("%s: not a .md file", rel)
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(p), ".md")
		raw, _ := os.ReadFile(p)
		doc := md.Parse(raw)
		if doc.Error != "" {
			t.Errorf("%s: frontmatter is not valid YAML: %s", rel, doc.Error)
		}
		generated := doc.Bool("generated")
		if stem == wiki.IndexTitle {
			if !generated || doc.Str("title") != "Index" {
				t.Errorf("%s: an Index page must be the generated one", rel)
			}
		} else if err := wiki.ValidTitle(stem); err != nil {
			t.Errorf("%s: file name is not a valid title: %v", rel, err)
		}
		if mdLink.MatchString(doc.Body) {
			t.Errorf("%s: links must be [[Title]] style, found a markdown file link", rel)
		}
		if generated { // every link of a generated page names an existing file name
			for _, l := range md.WikiLinks(doc.Body) {
				if _, err := os.Stat(filepath.Join(filepath.Dir(p), l+".md")); err != nil {
					t.Errorf("%s: [[%s]] has no file", rel, l)
				}
			}
		}
		key := strings.ToLower(filepath.Dir(rel) + "/" + stem)
		if names[key] {
			t.Errorf("%s: two files differ only in case", rel)
		}
		names[key] = true
	}
}
