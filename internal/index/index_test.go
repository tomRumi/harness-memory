package index

import (
	"database/sql"
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
	write(t, code, "data/rows.jsonl", "{\"row\": \"jsonl_marker_zz\"}\n")
	write(t, code, "export.csv", "a,b\ncsv_marker_zz,2\n")
	write(t, code, "big.json", "{\"k\": \""+strings.Repeat("x", 120000)+"\"}")
	write(t, code, "config.json", "{\"setting\": \"small_json_marker_zz\"}")
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
	if res.Added != 8 || res.Changed != 0 || res.Removed != 0 {
		t.Fatalf("first scan: %+v", res)
	}
	st, _ := d.Stats()
	want := map[string]int{"wiki/alpha": 3, "note/alpha": 2, "fact/global": 1, "code/alpha": 2}
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
	if res.Added+res.Changed+res.Removed != 0 || res.Unchanged != 8 {
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
	if !reflect.DeepEqual(got, []string{"config.json", "src/main.py"}) {
		t.Fatalf("code layer must hold only config.json and src/main.py, got %v", got)
	}
	for _, q := range []string{"should-never-be-indexed-12345", "TOKEN_VALUE", "BEGIN", "INSERT", "jsonl_marker_zz", "csv_marker_zz"} {
		if hits, _ := d.Keyword(q, Filter{}); len(hits) != 0 {
			t.Fatalf("%q reached the index: %+v", q, hits)
		}
	}
}

func TestAccentedQueriesFindAccentedAndPlainText(t *testing.T) {
	repo, _ := fixture(t)
	write(t, repo, "wiki/alpha/hu.md", "# Védelem\n\nAz árvíztűrő tükörfúrógép szabályai.\n")
	d := openDB(t)
	d.Rescan(repo)
	for _, q := range []string{"árvíztűrő", "arvizturo", "ÁRVÍZTŰRŐ"} {
		if hits, _ := d.Keyword(q, Filter{}); len(hits) == 0 || hits[0].NodeID != "wiki/alpha/hu" {
			t.Fatalf("query %q: %+v", q, hits)
		}
	}
}

func TestCodeRootProblemsNeverDeleteTheLayer(t *testing.T) {
	repo, code := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(code, link); err != nil {
		t.Fatal(err)
	}
	// a root that is a symlink works
	write(t, repo, "projects.yaml", "projects:\n  - name: alpha\n    code_roots: ['"+link+"']\n    ignore_dirs: [backups]\n")
	if res, err := d.Rescan(repo); err != nil || res.Removed != 0 {
		t.Fatalf("symlinked root: %+v %v", res, err)
	}
	// a missing root is an error and the code entries stay
	write(t, repo, "projects.yaml", "projects:\n  - name: alpha\n    code_roots: ['"+filepath.Join(code, "gone")+"']\n")
	if _, err := d.Rescan(repo); err == nil {
		t.Fatal("a missing code root must be an error")
	}
	st, _ := d.Stats()
	if st["code/alpha"] != 2 {
		t.Fatalf("code entries were lost: %v", st)
	}
	// a root that yields no files is an error
	empty := t.TempDir()
	write(t, repo, "projects.yaml", "projects:\n  - name: alpha\n    code_roots: ['"+empty+"']\n")
	if _, err := d.Rescan(repo); err == nil {
		t.Fatal("an empty code root must be an error")
	}
	// a relative root is relative to the memory repository, whatever the working directory is
	rel := filepath.Join(repo, "src-copy")
	write(t, rel, "a.py", "def relative_root_marker():\n    return 1\n")
	write(t, repo, "projects.yaml", "projects:\n  - name: beta\n    code_roots: ['src-copy']\n")
	cwd, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(cwd)
	if _, err := d.Rescan(repo); err != nil {
		t.Fatalf("relative root: %v", err)
	}
	if hits, _ := d.Keyword("relative_root_marker", Filter{Project: "beta"}); len(hits) != 1 {
		t.Fatalf("relative root not read: %+v", hits)
	}
}

func TestTwoRootsWithTheSamePathKeepTheFirstAndWarn(t *testing.T) {
	repo, _ := fixture(t)
	one, two := t.TempDir(), t.TempDir()
	write(t, one, "README.md", "readme one\n")
	write(t, two, "README.md", "readme two\n")
	write(t, repo, "projects.yaml", "projects:\n  - name: beta\n    code_roots: ['"+one+"', '"+two+"']\n")
	d := openDB(t)
	res, err := d.Rescan(repo)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "first code root wins")
	}
	if !found {
		t.Fatalf("no warning: %v", res.Warnings)
	}
	if hits, _ := d.Keyword("one", Filter{Project: "beta", Kinds: []string{"code"}}); len(hits) != 1 {
		t.Fatalf("first root must win: %+v", hits)
	}
	for _, bad := range []string{"a/b", "has space", ".hidden", ""} {
		write(t, repo, "projects.yaml", "projects:\n  - name: '"+bad+"'\n    code_roots: ['"+one+"']\n")
		if _, err := d.Rescan(repo); err == nil {
			t.Fatalf("project name %q must be refused", bad)
		}
	}
}

func TestUnreadablePathsAreReportedAndTheirNodesKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	repo, code := fixture(t)
	write(t, code, "sub/inner.py", "def inner_marker_fn():\n    pass\n")
	d := openDB(t)
	d.Rescan(repo)
	os.Chmod(filepath.Join(code, "sub"), 0)
	defer os.Chmod(filepath.Join(code, "sub"), 0o755)
	res, err := d.Rescan(repo)
	if err != nil {
		t.Fatalf("one unreadable directory must not stop the rescan: %v", err)
	}
	if res.Removed != 0 || len(res.Warnings) == 0 {
		t.Fatalf("expected a warning and no removal: %+v", res)
	}
	if hits, _ := d.Keyword("inner_marker_fn", Filter{}); len(hits) != 1 {
		t.Fatalf("node under the unreadable directory was dropped: %+v", hits)
	}
}

func TestMarkdownSymlinksAndHiddenWikiDirsAreNotIndexed(t *testing.T) {
	repo, _ := fixture(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("outside_link_marker text\n"), 0o644)
	os.Symlink(outside, filepath.Join(repo, "wiki/alpha/linked.md"))
	write(t, repo, "wiki/alpha/.obsidian/cfg.md", "hidden_dir_marker\n")
	d := openDB(t)
	d.Rescan(repo)
	for _, q := range []string{"outside_link_marker", "hidden_dir_marker"} {
		if hits, _ := d.Keyword(q, Filter{}); len(hits) != 0 {
			t.Fatalf("%q was indexed: %+v", q, hits)
		}
	}
}

func TestPrivateKeyFilesAndSecretNamesAreNeverIndexed(t *testing.T) {
	repo, code := fixture(t)
	write(t, code, "deploy.sh", "echo start\n"+"-----BEGIN RSA PRIV"+"ATE KEY-----\nabc\n")
	write(t, code, "wp-config.php", "<?php define('DB_NAME', 'wpconfig_marker');\n")
	write(t, code, "terraform.tfstate", "{\"tfstate_marker\": 1}\n")
	write(t, code, "creds/credentials.json", "{\"credentials_marker\": 1}\n")
	write(t, code, "settings.py", "API_"+"KEY = 'abcdefghijklmnop1234'\n")
	d := openDB(t)
	res, _ := d.Rescan(repo)
	for _, q := range []string{"wpconfig_marker", "tfstate_marker", "credentials_marker", "start"} {
		if hits, _ := d.Keyword(q, Filter{Kinds: []string{"code"}}); len(hits) != 0 {
			t.Fatalf("%q reached the index: %+v", q, hits)
		}
	}
	var keyWarn, credWarn bool
	for _, w := range res.Warnings {
		keyWarn = keyWarn || strings.Contains(w, "private key block")
		credWarn = credWarn || strings.Contains(w, "settings.py") && strings.Contains(w, "credential-like")
		if strings.Contains(w, "abcdefghijklmnop") {
			t.Fatalf("a warning printed the matched value: %s", w)
		}
	}
	if !keyWarn || !credWarn {
		t.Fatalf("expected a key-block warning and a credential-like warning: %v", res.Warnings)
	}
}

func TestLayerRootThatIsASymlinkKeepsItsNodes(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	before, _ := d.Stats()
	real := filepath.Join(t.TempDir(), "wiki-real")
	os.Rename(filepath.Join(repo, "wiki"), real)
	if err := os.Symlink(real, filepath.Join(repo, "wiki")); err != nil {
		t.Fatal(err)
	}
	res, err := d.Rescan(repo)
	if err != nil || res.Removed != 0 || res.Added != 0 {
		t.Fatalf("a symlinked wiki/ must read like a directory: %+v %v", res, err)
	}
	if after, _ := d.Stats(); !reflect.DeepEqual(before, after) {
		t.Fatalf("stats changed: %v -> %v", before, after)
	}
}

func TestEmptyCodeRootIsRefused(t *testing.T) {
	repo, _ := fixture(t)
	write(t, repo, "projects.yaml", "projects:\n  - name: alpha\n    code_roots: ['']\n")
	d := openDB(t)
	if _, err := d.Rescan(repo); err == nil || !strings.Contains(err.Error(), "empty code root") {
		t.Fatalf("%v", err)
	}
}

func TestDecomposedAccentsAndHiddenMarkdown(t *testing.T) {
	repo, _ := fixture(t)
	write(t, repo, "wiki/alpha/hu2.md", "# Őrzés\n\nAz őrzés szabályai.\n")
	write(t, repo, "wiki/alpha/.draft.md", "draft_hidden_marker\n")
	d := openDB(t)
	d.Rescan(repo)
	nfd := "o\u030brze\u0301s" // "őrzés" written with combining marks
	if hits, _ := d.Keyword(nfd, Filter{}); len(hits) == 0 || hits[0].NodeID != "wiki/alpha/hu2" {
		t.Fatalf("decomposed query: %+v", hits)
	}
	if hits, _ := d.Keyword("draft_hidden_marker", Filter{}); len(hits) != 0 {
		t.Fatalf("hidden markdown indexed: %+v", hits)
	}
}

func TestPGPAndPuTTYKeysAreNotIndexed(t *testing.T) {
	repo, code := fixture(t)
	write(t, code, "pgp.txt", "-----BEGIN PGP PRIVATE"+" KEY BLOCK-----\npgp_body_marker\n")
	write(t, code, "putty.txt", "PuTTY-User-"+"Key-File-3: ssh-rsa\nputty_body_marker\n")
	write(t, code, "key.ppk", "ppk_body_marker\n")
	d := openDB(t)
	d.Rescan(repo)
	for _, q := range []string{"pgp_body_marker", "putty_body_marker", "ppk_body_marker"} {
		if hits, _ := d.Keyword(q, Filter{}); len(hits) != 0 {
			t.Fatalf("%q was indexed", q)
		}
	}
}

func TestUnchangedFilesAreNotChunkedAgain(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	parseCalls.Store(0)
	res, _ := d.Rescan(repo)
	if res.Unchanged != 8 || parseCalls.Load() != 0 {
		t.Fatalf("%+v parsed %d files", res, parseCalls.Load())
	}
	write(t, repo, "wiki/alpha/setup.md", "# Setup\n\nchanged text here\n")
	d.Rescan(repo)
	if parseCalls.Load() != 1 {
		t.Fatalf("expected one parse, got %d", parseCalls.Load())
	}
}

func TestKeyBlockInMarkdownIsNotIndexed(t *testing.T) {
	repo, _ := fixture(t)
	write(t, repo, "wiki/alpha/keys.md", "# Keys\n\n-----BEGIN PGP PRIVATE"+" KEY BLOCK-----\nmd_key_body_marker\n")
	d := openDB(t)
	res, _ := d.Rescan(repo)
	if hits, _ := d.Keyword("md_key_body_marker", Filter{}); len(hits) != 0 {
		t.Fatalf("a key block in markdown was indexed: %+v", hits)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "wiki/alpha/keys") && strings.Contains(w, "private key block")
	}
	if !found {
		t.Fatalf("no warning: %v", res.Warnings)
	}
}

func TestBrokenLayerLinkAndRepoRootAreRefused(t *testing.T) {
	repo, _ := fixture(t)
	d := openDB(t)
	d.Rescan(repo)
	before, _ := d.Stats()
	// wiki/ becomes a link to a volume that is not there
	os.Rename(filepath.Join(repo, "wiki"), filepath.Join(repo, "wiki-away"))
	os.Symlink(filepath.Join(t.TempDir(), "not-mounted"), filepath.Join(repo, "wiki"))
	if _, err := d.Rescan(repo); err == nil {
		t.Fatal("a broken wiki/ link must be an error")
	}
	if after, _ := d.Stats(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused rescan changed the index: %v -> %v", before, after)
	}
	os.Remove(filepath.Join(repo, "wiki"))
	os.Rename(filepath.Join(repo, "wiki-away"), filepath.Join(repo, "wiki"))
	// "." and the repository path itself are not valid code roots
	for _, root := range []string{".", repo, "./", "..", "wiki", filepath.Dir(repo)} {
		write(t, repo, "projects.yaml", "projects:\n  - name: alpha\n    code_roots: ['"+root+"']\n")
		if _, err := d.Rescan(repo); err == nil {
			t.Fatalf("code root %q must be refused", root)
		}
	}
}

func TestOpenReadOnlyNeverCreatesOrMigrates(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.db")
	if _, err := OpenReadOnly(missing); err == nil {
		t.Fatal("a missing index must be an error")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("OpenReadOnly created the file")
	}
	// an index of an older schema is refused and left exactly as it was
	path := filepath.Join(dir, "old.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO nodes(id,kind,project,path,stem,title,status,retired_reason,superseded_by,pinned,note_kind,created_at,updated_at,content_hash) VALUES('wiki/p/a','wiki','p','wiki/p/a.md','a','A','active','','',0,'','','','h')`); err != nil {
		t.Fatal(err)
	}
	d.Exec(`PRAGMA user_version = 3`)
	d.Close()
	if _, err := OpenReadOnly(path); err == nil || !strings.Contains(err.Error(), "rescan") {
		t.Fatalf("an index of another schema must be refused with the cure: %v", err)
	}
	raw, _ := sql.Open("sqlite", "file:"+path+"?mode=ro")
	var n, v int
	raw.QueryRow(`SELECT count(*) FROM nodes`).Scan(&n)
	raw.QueryRow(`PRAGMA user_version`).Scan(&v)
	raw.Close()
	if n != 1 || v != 3 {
		t.Fatalf("the refused open changed the index: %d nodes, version %d", n, v)
	}
	// a current index opens and reads, and cannot be written through this handle
	cur := filepath.Join(dir, "cur.db")
	c, _ := Open(cur)
	c.Close()
	ro, err := OpenReadOnly(cur)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`INSERT INTO meta(key,value) VALUES('x','y')`); err == nil {
		t.Fatal("a read-only handle accepted a write")
	}
}
