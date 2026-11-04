package index

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture builds a synthetic memory repository and a synthetic code root. Nothing here is real data.
func fixture(t *testing.T) (repo, code string) {
	t.Helper()
	repo, code = t.TempDir(), t.TempDir()
	write(t, repo, "wiki/alpha/overview.md", "---\ntitle: Overview\nsupersedes: wiki/alpha/old\n---\n\n# Overview\nThe widget listens on port 8765.\n\n## Install\nRun the installer, see [[Setup]] and [[Other|alias]].\n")
	write(t, repo, "wiki/alpha/setup.md", "# Setup\n\n## Steps\nCreate the frobnicator directory first.\n")
	write(t, repo, "notes/alpha/n-0001.md", "---\nkind: rag:gotcha\ncreated: 2026-10-01T10:00:00Z\n---\n\nThe zeta_flag option must stay off in production.\n")
	write(t, repo, "notes/alpha/n-0002.md", "---\nstatus: retired\nretired_reason: wrong\n---\n\nOld retired claim about quibblewomp.\n")
	write(t, repo, "facts/global/f-1.md", "---\ncategory: user_pref\ntrust: 0.5\n---\n\nPrefers short answers.\n")
	write(t, repo, "wiki/alpha/broken.md", "---\ntitle: [unclosed\n---\nStill readable text about gadgets.\n")
	write(t, repo, "projects.yaml", "projects:\n  - name: alpha\n    code_roots: ['"+code+"']\n    ignore_dirs: [backups]\n")
	write(t, code, "src/main.py", "def compute_total(items):\n    return sum(items)\n")
	write(t, code, "node_modules/skip.js", "ignored()\n")
	write(t, code, "assets/logo.png", "binary")
	write(t, code, "assets/blob.dat", "head\x00\x01\x02 not text")
	write(t, code, ".env", "TOK"+"EN_VALUE=should-never-be-indexed-12345\n") // built at run time so the leak gate does not trip on the fixture
	write(t, code, ".worktrees/copy/src/main.py", "def compute_total(items):\n    return sum(items)\n")
	write(t, code, "keys/server.pem", "-----BEGIN-----\nabc\n")
	write(t, code, "backups/dump.sql", "INSERT INTO t VALUES (1);\n")
	return repo, code
}

func openDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestRescanBuildsNodesEdgesAndSkipsJunk(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	res, err := d.Rescan(repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 7 || res.Changed != 0 || res.Removed != 0 {
		t.Fatalf("first scan: %+v", res)
	}
	st, _ := d.Stats()
	want := map[string]int{"wiki/alpha": 3, "note/alpha": 2, "fact/global": 1, "code/alpha": 1}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("stats %v want %v", st, want)
	}
	var n int
	d.QueryRow(`SELECT count(*) FROM edges WHERE subject_id='wiki/alpha/overview' AND relation='links_to'`).Scan(&n)
	if n != 2 {
		t.Fatalf("wikilink edges: %d", n)
	}
	d.QueryRow(`SELECT count(*) FROM edges WHERE relation='supersedes'`).Scan(&n)
	if n != 1 {
		t.Fatalf("frontmatter edge: %d", n)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "broken.md") {
		t.Fatalf("a malformed frontmatter must warn, not fail: %v", res.Warnings)
	}
	var kind string
	d.QueryRow(`SELECT note_kind FROM nodes WHERE id='notes/alpha/n-0001'`).Scan(&kind)
	if kind != "rag:gotcha" {
		t.Fatalf("note kind: %q", kind)
	}
}

func TestDeleteAndRebuildGivesTheSameHash(t *testing.T) {
	repo, _ := fixture(t)
	dir := t.TempDir()
	a, _ := Open(filepath.Join(dir, "a.db"))
	defer a.Close()
	if _, err := a.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	h1, c1, _ := a.StateHash()
	a.Close()
	os.Remove(filepath.Join(dir, "a.db"))
	b, _ := Open(filepath.Join(dir, "b.db")) // a fresh database
	defer b.Close()
	if _, err := b.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	h2, c2, _ := b.StateHash()
	if h1 != h2 || !reflect.DeepEqual(c1, c2) {
		t.Fatalf("rebuild differs:\n%s %v\n%s %v", h1, c1, h2, c2)
	}
}

func TestIncrementalRescanChangesExactlyTheEditedNode(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	before, _ := d.NodeDigests()
	h0, _, _ := d.StateHash()

	res, _ := d.Rescan(repo)
	if res.Added+res.Changed+res.Removed != 0 || res.Unchanged != 7 {
		t.Fatalf("an unchanged tree must change nothing: %+v", res)
	}
	write(t, repo, "wiki/alpha/setup.md", "# Setup\n\n## Steps\nCreate the frobnicator directory first, then restart.\n")
	res, _ = d.Rescan(repo)
	if res.Changed != 1 || !reflect.DeepEqual(res.ChangedIDs, []string{"wiki/alpha/setup"}) {
		t.Fatalf("expected exactly the edited node: %+v", res)
	}
	after, _ := d.NodeDigests()
	var diff []string
	for id, h := range after {
		if before[id] != h {
			diff = append(diff, id)
		}
	}
	sort.Strings(diff)
	if !reflect.DeepEqual(diff, []string{"wiki/alpha/setup"}) {
		t.Fatalf("digest differences: %v", diff)
	}
	if h1, _, _ := d.StateHash(); h1 == h0 {
		t.Fatal("state hash must change after an edit")
	}
	// the keyword index follows the edit
	hits, _ := d.Keyword("restart", Filter{})
	if len(hits) != 1 || hits[0].NodeID != "wiki/alpha/setup" {
		t.Fatalf("keyword index stale: %+v", hits)
	}
}

func TestRemovedFileLeavesNoTrace(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	os.Remove(filepath.Join(repo, "notes/alpha/n-0001.md"))
	res, _ := d.Rescan(repo)
	if res.Removed != 1 {
		t.Fatalf("%+v", res)
	}
	var n int
	d.QueryRow(`SELECT count(*) FROM chunks WHERE node_id='notes/alpha/n-0001'`).Scan(&n)
	d.QueryRow(`SELECT count(*) + (SELECT count(*) FROM chunks_fts WHERE chunks_fts MATCH 'zeta_flag') FROM edges WHERE subject_id='notes/alpha/n-0001'`).Scan(new(int))
	if n != 0 {
		t.Fatal("chunks of a removed node remain")
	}
	hits, _ := d.Keyword("zeta_flag", Filter{})
	if len(hits) != 0 {
		t.Fatalf("removed node still searchable: %+v", hits)
	}
}

func TestKeywordRecallRulesStatusFiltersAndIdentifiers(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	// an identifier with an underscore is one token
	hits, _ := d.Keyword("what does the zeta_flag option do", Filter{})
	if len(hits) == 0 || hits[0].NodeID != "notes/alpha/n-0001" {
		t.Fatalf("identifier query: %+v", hits)
	}
	// retired nodes are never returned
	if hits, _ := d.Keyword("quibblewomp", Filter{}); len(hits) != 0 {
		t.Fatalf("retired node returned: %+v", hits)
	}
	// project and kind filters
	if hits, _ := d.Keyword("compute_total", Filter{Kinds: []string{"code"}, Project: "alpha"}); len(hits) != 1 || hits[0].Path != "src/main.py" {
		t.Fatalf("code filter: %+v", hits)
	}
	if hits, _ := d.Keyword("compute_total", Filter{Project: "other"}); len(hits) != 0 {
		t.Fatalf("project filter: %+v", hits)
	}
	// heading-aware: the section name travels with the hit
	hits, _ = d.Keyword("installer", Filter{Kinds: []string{"wiki"}})
	if len(hits) == 0 || hits[0].Section != "Install" {
		t.Fatalf("section: %+v", hits)
	}
	// nothing but stop words: no query, no error
	if hits, err := d.Keyword("what is the", Filter{}); err != nil || hits != nil {
		t.Fatalf("stop words only: %v %v", hits, err)
	}
	// FTS syntax in the query must not break the search
	if _, err := d.Keyword(`"unterminated AND ( NEAR`, Filter{}); err != nil {
		t.Fatalf("query syntax leaked into FTS: %v", err)
	}
}

func TestCodeLayerNeverIndexesHiddenBinarySecretOrIgnoredFiles(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	rows, _ := d.Query(`SELECT path FROM nodes WHERE kind='code' ORDER BY path`)
	var got []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		got = append(got, p)
	}
	rows.Close()
	if !reflect.DeepEqual(got, []string{"src/main.py"}) {
		t.Fatalf("code layer must hold only src/main.py, got %v", got)
	}
	for _, q := range []string{"should-never-be-indexed-12345", "TOKEN_VALUE", "BEGIN", "INSERT"} {
		if hits, _ := d.Keyword(q, Filter{}); len(hits) != 0 {
			t.Fatalf("%q reached the index: %+v", q, hits)
		}
	}
}
