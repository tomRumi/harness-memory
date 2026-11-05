package index

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func scanned(t *testing.T, files map[string]string) (*DB, string) {
	t.Helper()
	repo := t.TempDir()
	for rel, c := range files {
		write(t, repo, rel, c)
	}
	d := openDB(t)
	if _, err := d.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	return d, repo
}

// linkOf returns the state of the body link `ref` written in node `from`.
func linkOf(t *testing.T, d *DB, from, ref string) (status, resolved string) {
	t.Helper()
	if err := d.QueryRow(`SELECT status, resolved FROM edges WHERE subject_id=? AND relation='links_to' AND source='body' AND object_ref=?`, from, ref).Scan(&status, &resolved); err != nil {
		t.Fatalf("no link %q in %s: %v", ref, from, err)
	}
	return
}

func TestLinkResolution(t *testing.T) {
	d, _ := scanned(t, map[string]string{
		"wiki/a/Source.md":      "See [[Shared]], [[Only B]], [[Twin]], [[b/Shared]], [[shared|shown text]], [[Shared#Heading]], [[Short]], [[SHARED]], [[Missing]], [[a/Missing]], [[folder/Deep]], [[Deep]].\n",
		"wiki/a/Shared.md":      "same project\n",
		"wiki/b/Shared.md":      "other project\n",
		"wiki/b/Only B.md":      "---\naliases: [Short, Twin]\n---\nonly in b\n",
		"wiki/c/Twin.md":        "also named twin\n",
		"wiki/a/folder/Deep.md": "deep page\n",
	})
	cases := []struct{ ref, status, to string }{
		{"Shared", "ok", "wiki/a/Shared"},           // same project first, although b has one too
		{"SHARED", "ok", "wiki/a/Shared"},           // case-insensitive
		{"shared", "ok", "wiki/a/Shared"},           // |shown text and #Heading are not part of the target
		{"Only B", "ok", "wiki/b/Only B"},           // unique in another project
		{"b/Shared", "ok", "wiki/b/Shared"},         // explicit project
		{"Short", "ok", "wiki/b/Only B"},            // by alias
		{"Twin", "ambiguous", ""},                   // c/Twin by name and b/Only B by alias, none in a
		{"Missing", "broken", ""},                   // nothing
		{"a/Missing", "broken", ""},                 // explicit project, no such page
		{"folder/Deep", "ok", "wiki/a/folder/Deep"}, // path inside the project
		{"Deep", "ok", "wiki/a/folder/Deep"},        // file name in a sub-folder
	}
	for _, c := range cases {
		if st, to := linkOf(t, d, "wiki/a/Source", c.ref); st != c.status || to != c.to {
			t.Errorf("[[%s]] = %s %q, want %s %q", c.ref, st, to, c.status, c.to)
		}
	}
	ps, err := d.LinkProblems("a")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.Status+" "+p.Page+" "+p.Link)
	}
	want := []string{"broken wiki/a/Source.md Missing", "broken wiki/a/Source.md a/Missing", "ambiguous wiki/a/Source.md Twin"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("problems %v want %v", got, want)
	}
	for _, p := range ps {
		if p.Link == "Twin" && !reflect.DeepEqual(p.Matches, []string{"wiki/b/Only B.md", "wiki/c/Twin.md"}) {
			t.Fatalf("matches: %v", p.Matches)
		}
	}
	if all, _ := d.LinkProblems(""); len(all) != 3 {
		t.Fatalf("all projects: %v", all)
	}
	if other, _ := d.LinkProblems("b"); len(other) != 0 {
		t.Fatalf("project b has no problems: %v", other)
	}
	if b, a, _ := d.LinkCounts(); b != 2 || a != 1 {
		t.Fatalf("counts %d broken %d ambiguous", b, a)
	}
}

func TestLinkInAnotherProjectWithTwoMatchesIsAmbiguousNotSameProject(t *testing.T) {
	d, _ := scanned(t, map[string]string{
		"wiki/a/Src.md": "[[Dup]]\n",
		"wiki/b/Dup.md": "x\n",
		"wiki/c/Dup.md": "y\n",
	})
	if st, _ := linkOf(t, d, "wiki/a/Src", "Dup"); st != "ambiguous" {
		t.Fatalf("status %s", st)
	}
	// two pages with one name in the same project are ambiguous too
	d2, _ := scanned(t, map[string]string{"wiki/a/Src.md": "[[Dup]]\n", "wiki/a/x/Dup.md": "1\n", "wiki/a/y/Dup.md": "2\n"})
	if st, _ := linkOf(t, d2, "wiki/a/Src", "Dup"); st != "ambiguous" {
		t.Fatalf("same project: %s", st)
	}
}

func TestRescanRecomputesLinksWhenPagesAppearVanishOrMove(t *testing.T) {
	d, repo := scanned(t, map[string]string{"wiki/a/Src.md": "[[Later]] and [[Moved]]\n", "wiki/a/Moved.md": "m\n"})
	if st, _ := linkOf(t, d, "wiki/a/Src", "Later"); st != "broken" {
		t.Fatalf("before: %s", st)
	}
	if b, _, _ := d.LinkCounts(); b != 1 {
		t.Fatalf("an unresolved link stays reported: %d", b)
	}
	write(t, repo, "wiki/a/Later.md", "now there\n")
	d.Rescan(repo)
	if st, to := linkOf(t, d, "wiki/a/Src", "Later"); st != "ok" || to != "wiki/a/Later" {
		t.Fatalf("after the page appears: %s %s", st, to)
	}
	// rename: the old name breaks, an alias keeps the link alive, the new file name works as well
	os.Rename(filepath.Join(repo, "wiki/a/Moved.md"), filepath.Join(repo, "wiki/a/Renamed.md"))
	d.Rescan(repo)
	if st, _ := linkOf(t, d, "wiki/a/Src", "Moved"); st != "broken" {
		t.Fatalf("after rename: %s", st)
	}
	write(t, repo, "wiki/a/Renamed.md", "---\naliases: [Moved]\n---\nm\n")
	d.Rescan(repo)
	if st, to := linkOf(t, d, "wiki/a/Src", "Moved"); st != "ok" || to != "wiki/a/Renamed" {
		t.Fatalf("alias: %s %s", st, to)
	}
	os.Remove(filepath.Join(repo, "wiki/a/Later.md"))
	d.Rescan(repo)
	if st, _ := linkOf(t, d, "wiki/a/Src", "Later"); st != "broken" {
		t.Fatalf("after delete: %s", st)
	}
}

func TestUpsertAlsoResolvesLinks(t *testing.T) {
	d, repo := scanned(t, map[string]string{"wiki/a/Src.md": "[[Late]]\n"})
	write(t, repo, "wiki/a/Late.md", "x\n")
	if err := d.Upsert(repo, "wiki/a/Late.md"); err != nil {
		t.Fatal(err)
	}
	if st, _ := linkOf(t, d, "wiki/a/Src", "Late"); st != "ok" {
		t.Fatalf("after upsert: %s", st)
	}
	os.Remove(filepath.Join(repo, "wiki/a/Late.md"))
	d.Upsert(repo, "wiki/a/Late.md")
	if st, _ := linkOf(t, d, "wiki/a/Src", "Late"); st != "broken" {
		t.Fatalf("after upsert of a deleted file: %s", st)
	}
}

func TestLinksInNotesResolveLikeWikiLinksButAreNotReported(t *testing.T) {
	d, _ := scanned(t, map[string]string{
		"wiki/a/Page.md": "p\n",
		"notes/a/n-1.md": "See [[page]] and [[nothing here]].\n",
	})
	if st, to := linkOf(t, d, "notes/a/n-1", "page"); st != "ok" || to != "wiki/a/Page" {
		t.Fatalf("%s %s", st, to)
	}
	if st, _ := linkOf(t, d, "notes/a/n-1", "nothing here"); st != "broken" {
		t.Fatal(st)
	}
	if ps, _ := d.LinkProblems(""); len(ps) != 0 {
		t.Fatalf("only wiki pages are reported: %v", ps)
	}
	if bl, _ := d.Backlinks("wiki/a/Page"); len(bl) != 0 {
		t.Fatalf("backlinks list pages, not notes: %v", bl)
	}
}

func TestBacklinksAndOutLinks(t *testing.T) {
	d, _ := scanned(t, map[string]string{
		"wiki/a/Hub.md":      "[[Leaf One]] [[Leaf Two]] [[Hub]] [[gone]]\n",
		"wiki/a/Leaf One.md": "back to [[Hub]]\n",
		"wiki/b/Leaf Two.md": "also [[a/Hub]]\n",
		"wiki/b/Alone.md":    "no links\n",
	})
	out, _ := d.OutLinks("wiki/a/Hub")
	if !reflect.DeepEqual(out, []string{"wiki/a/Leaf One", "wiki/b/Leaf Two"}) {
		t.Fatalf("out: %v", out)
	}
	back, _ := d.Backlinks("wiki/a/Hub")
	if !reflect.DeepEqual(back, []string{"wiki/a/Leaf One", "wiki/b/Leaf Two"}) {
		t.Fatalf("back: %v", back)
	}
	if b, _ := d.Backlinks("wiki/b/Alone"); len(b) != 0 {
		t.Fatal(b)
	}
}

func TestResolveTitle(t *testing.T) {
	d, _ := scanned(t, map[string]string{"wiki/a/One.md": "x\n", "wiki/a/Dup.md": "x\n", "wiki/b/Dup.md": "x\n", "wiki/b/Alias.md": "---\naliases: [Nick]\n---\nx\n"})
	for name, want := range map[string]string{"one": "wiki/a/One", "ONE.md": "wiki/a/One", "b/dup": "wiki/b/Dup", "nick": "wiki/b/Alias", "Alias": "wiki/b/Alias"} {
		if id, m, err := d.ResolveTitle(name); err != nil || id != want || m != nil {
			t.Errorf("%q: %q %v %v", name, id, m, err)
		}
	}
	if id, m, _ := d.ResolveTitle("Dup"); id != "" || !reflect.DeepEqual(m, []string{"wiki/a/Dup", "wiki/b/Dup"}) {
		t.Errorf("Dup: %q %v", id, m)
	}
	if id, m, _ := d.ResolveTitle("none"); id != "" || m != nil {
		t.Errorf("none: %q %v", id, m)
	}
}

func TestStateHashSurvivesDeletingTheIndexWithLinks(t *testing.T) {
	files := map[string]string{
		"wiki/a/Src.md":    "[[Shared]] [[Only B]] [[Twin]] [[Missing]]\n",
		"wiki/a/Shared.md": "x\n", "wiki/b/Shared.md": "y\n",
		"wiki/b/Only B.md": "---\naliases: [Twin]\n---\nz\n", "wiki/c/Twin.md": "w\n",
	}
	repo := t.TempDir()
	for rel, c := range files {
		write(t, repo, rel, c)
	}
	dir := t.TempDir()
	hash := func(name string) string {
		d, err := Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.Rescan(repo); err != nil {
			t.Fatal(err)
		}
		h, _, err := d.StateHash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h1 := hash("index.db")
	os.Remove(filepath.Join(dir, "index.db"))
	os.Remove(filepath.Join(dir, "index.db-wal"))
	os.Remove(filepath.Join(dir, "index.db-shm"))
	if h2 := hash("index.db"); h1 != h2 {
		t.Fatalf("hash after delete and rescan differs: %s %s", h1, h2)
	}
	// incremental scans agree with a scan from nothing
	d, _ := Open(filepath.Join(dir, "inc.db"))
	defer d.Close()
	d.Rescan(repo)
	write(t, repo, "wiki/a/Missing.md", "now there\n")
	d.Rescan(repo)
	os.Remove(filepath.Join(repo, "wiki/a/Missing.md"))
	d.Rescan(repo)
	if hi, _, _ := d.StateHash(); hi != h1 {
		t.Fatal("an incremental history must end in the same state as a fresh scan")
	}
}

func TestHandEditOfOnePageChangesExactlyOneDigest(t *testing.T) {
	d, repo := scanned(t, map[string]string{
		"wiki/a/Src.md": "[[Target]] [[Missing]]\n", "wiki/a/Target.md": "target text\n", "wiki/b/Other.md": "[[Target]]\n",
	})
	before, _ := d.NodeDigests()
	write(t, repo, "wiki/a/Target.md", "target text, edited by hand\n")
	d.Rescan(repo)
	after, _ := d.NodeDigests()
	var diff []string
	for id, h := range after {
		if before[id] != h {
			diff = append(diff, id)
		}
	}
	if !reflect.DeepEqual(diff, []string{"wiki/a/Target"}) {
		t.Fatalf("digests that changed: %v", diff)
	}
}

func TestPageFieldsAreStoredAndTitleIsFrontmatterOrFileName(t *testing.T) {
	d, _ := scanned(t, map[string]string{
		"wiki/a/Plain Name.md":    "# A Heading That Is Not The Title\n\nFirst sentence here. Second one.\n",
		"wiki/a/File.md":          "---\ntitle: Frontmatter Title\nsummary: Says what it is\naliases: [Other Name, Third]\n---\nbody\n",
		"wiki/a/Őrzés és Café.md": "Szöveg.\n",
	})
	get := func(id string) (title, summary, aliases string) {
		d.QueryRow(`SELECT title, summary, aliases FROM nodes WHERE id=?`, id).Scan(&title, &summary, &aliases)
		return
	}
	if ti, su, _ := get("wiki/a/Plain Name"); ti != "Plain Name" || su != "First sentence here." {
		t.Fatalf("plain: %q %q", ti, su)
	}
	if ti, su, al := get("wiki/a/File"); ti != "Frontmatter Title" || su != "Says what it is" || al != "Other Name\nThird" {
		t.Fatalf("file: %q %q %q", ti, su, al)
	}
	if ti, _, _ := get("wiki/a/Őrzés és Café"); ti != "Őrzés és Café" {
		t.Fatalf("unicode: %q", ti)
	}
}

func TestBuildIndexPageIsCompleteDeterministicAndExcludesItself(t *testing.T) {
	files := map[string]string{
		"wiki/p/zeta.md":       "---\nsummary: Zeta summary\n---\nignored body.\n",
		"wiki/p/Alpha.md":      "# Alpha\n\nAlpha has no summary. More text.\n",
		"wiki/p/Index.md":      "---\ntitle: Index\ngenerated: true\n---\n# Index\n\n- [[Alpha]]\n",
		"wiki/p/sub/Deep.md":   "Deep one\n",
		"wiki/p/Éclair.md":     "---\nsummary: " + strings.Repeat("long ", 50) + "\n---\nx\n",
		"wiki/p/Empty page.md": "# Just a heading\n",
		"wiki/q/Other.md":      "other project\n",
	}
	d, _ := scanned(t, files)
	a, err := d.BuildIndexPage("p")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := d.BuildIndexPage("p")
	if a != b {
		t.Fatal("not deterministic")
	}
	var lines []string
	for _, ln := range strings.Split(a, "\n") {
		if strings.HasPrefix(ln, "- ") {
			lines = append(lines, ln)
		}
	}
	if len(lines) != 5 {
		t.Fatalf("5 pages expected (not Index, not project q): %d\n%s", len(lines), a)
	}
	wantStart := []string{"- [[Alpha]] - Alpha has no summary.", "- [[Deep]] - Deep one", "- [[Empty page]]", "- [[zeta]] - Zeta summary", "- [[Éclair]] - long long"}
	for i, w := range wantStart {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], w)
		}
	}
	if strings.Contains(a, "[[Index]]") || strings.Contains(a, "Other") {
		t.Fatalf("Index lists itself or another project:\n%s", a)
	}
	if r := []rune(strings.TrimPrefix(lines[4], "- [[Éclair]] - ")); len(r) != 140 || r[139] != '…' {
		t.Fatalf("summary cap: %d %q", len(r), string(r))
	}
	if !strings.HasPrefix(a, "---\ngenerated: true\ntitle: Index\n---\n\n# Index\n") {
		t.Fatalf("head:\n%s", a)
	}
}
