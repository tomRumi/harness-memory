package index

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/tomRumi/harness-memory-service/internal/chunk"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"go.yaml.in/yaml/v3"
)

// Relations that frontmatter may declare. Anything else in frontmatter is ignored by the edge extractor.
var Relations = []string{"supersedes", "depends_on", "part_of", "produced_by", "links_to"}

// Project is one entry of projects.yaml in the memory repository.
type Project struct {
	Name       string   `yaml:"name"`
	CodeRoots  []string `yaml:"code_roots"`
	IgnoreDirs []string `yaml:"ignore_dirs"` // directory names skipped anywhere under a code root (dumps, backups, archives)
}

type projectsFile struct {
	Projects []Project `yaml:"projects"`
}

// LoadProjects reads <repo>/projects.yaml; a missing file means no projects with code roots.
func LoadProjects(repo string) ([]Project, error) {
	b, err := os.ReadFile(filepath.Join(repo, "projects.yaml"))
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var pf projectsFile
	if err := yaml.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("projects.yaml: %w", err)
	}
	return pf.Projects, nil
}

// Result reports what a rescan did.
type Result struct {
	Added, Changed, Removed, Unchanged int
	ChangedIDs                         []string
	Warnings                           []string
}

type parsed struct {
	node   nodeRow
	chunks []chunk.Piece
	edges  []edge
}

type nodeRow struct {
	ID, Kind, Project, Path, Title, Status, RetiredReason, SupersededBy, NoteKind, Created, Updated, Hash string
	Pinned                                                                                                bool
}

type edge struct{ Relation, Object, Source string }

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Skip lists carried over from the service this one replaces (server.py ingest_code).
var (
	skipParts = map[string]bool{".git": true, ".venv": true, "venv": true, "__pycache__": true, ".pytest_cache": true,
		"node_modules": true, "dist": true, "build": true, "target": true, ".idea": true, ".vscode": true, "logs": true, ".hermes": true,
		"chroma": true, "chroma-agent-workflows": true, ".ollama": true}
	skipSuffix = map[string]bool{".pyc": true, ".pyo": true, ".so": true, ".dylib": true, ".dll": true, ".exe": true, ".class": true,
		".jar": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".pdf": true, ".zip": true, ".tar": true,
		".gz": true, ".whl": true, ".db": true, ".db-wal": true, ".db-shm": true, ".lock": true, ".log": true, ".map": true, ".svg": true,
		".sqlite": true, ".sqlite3": true}
	skipBase = map[string]bool{"package-lock.json": true, "yarn.lock": true, "poetry.lock": true, "pdm.lock": true, "uv.lock": true, "composer.lock": true}
)

// secretSuffix names files that usually hold credentials or keys. They are never indexed, because everything
// indexed can be returned to an agent.
var secretSuffix = map[string]bool{".pem": true, ".key": true, ".p12": true, ".pfx": true, ".kdbx": true, ".jks": true, ".keystore": true,
	".tfstate": true, ".tfvars": true, ".ppk": true, ".gpg": true, ".asc": true}

// secretName lists whole file names that hold credentials in the common frameworks and tools.
var secretName = map[string]bool{"wp-config.php": true, "auth.json": true, "service-account.json": true, "kubeconfig": true,
	"htpasswd": true, "netrc": true, "npmrc": true, "pypirc": true}

// dataSuffix names tabular and record formats. They are data, not code: a recall for a function name would
// only ever hit rows of them, and they make up most of the chunks of a working tree.
var dataSuffix = map[string]bool{".jsonl": true, ".ndjson": true, ".csv": true, ".tsv": true, ".snap": true, ".parquet": true}

// maxJSONBytes is the size above which a .json file is treated as data rather than configuration.
const maxJSONBytes = 100_000

func skipCode(rel string, ignore map[string]bool) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if strings.HasPrefix(p, ".") { // hidden directories and hidden files (.git, .env, .claude, .worktrees ...)
			return true
		}
	}
	for _, p := range parts[:len(parts)-1] {
		if skipParts[p] || ignore[p] {
			return true
		}
	}
	base := parts[len(parts)-1]
	low := strings.ToLower(base)
	if secretSuffix[filepath.Ext(low)] || strings.HasPrefix(low, "id_rsa") || strings.HasPrefix(low, "id_ed25519") ||
		secretName[low] || strings.HasPrefix(low, "secret") || strings.HasPrefix(low, "credential") {
		return true
	}
	if skipBase[base] || strings.HasSuffix(low, ".min.js") || strings.HasSuffix(low, ".min.css") {
		return true
	}
	if skipSuffix[strings.ToLower(filepath.Ext(base))] || dataSuffix[strings.ToLower(filepath.Ext(base))] {
		return true
	}
	if strings.HasSuffix(low, ".json") {
		for _, seg := range parts[:len(parts)-1] {
			l := strings.ToLower(seg)
			if strings.Contains(l, "cache") || l == "db" || l == "data" {
				return true
			}
		}
		if strings.Contains(low, "cache") {
			return true
		}
	}
	return false
}

// cand is one file that may become a node. Nothing is read until Rescan decides it has to.
type cand struct {
	id, kind, project, rel, abs string // kind is wiki, notes, facts or code
}

// maxCodeBytes bounds what is read from one code file; a larger file is data, not source.
const maxCodeBytes = 2_000_000

// resolveRoot makes a code root absolute (a relative root is relative to the memory repository, not to the
// working directory) and follows a symlink, so a root that is a link works.
func resolveRoot(repo, root string) (string, error) {
	if strings.HasPrefix(root, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(h, root[2:])
		}
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(repo, root)
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return r, nil
}

// gather lists the candidate files. Unreadable directories are reported and their node ids are returned in
// keep, so a permission problem never deletes what the index already holds.
func gather(repo string) (cands []cand, warns []string, keep []string, err error) {
	for _, kind := range []string{"wiki", "notes", "facts"} {
		root := filepath.Join(repo, kind)
		if fi, lerr := os.Lstat(root); lerr == nil && fi.Mode()&os.ModeSymlink != 0 { // wiki/, notes/ or facts/ may be a link
			r, rerr := filepath.EvalSymlinks(root)
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("%s is a link that does not lead anywhere (is a shared volume not mounted?): refusing to rescan, nothing was removed", kind)
			}
			root = r
		}
		werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return filepath.SkipDir
				}
				inside, _ := filepath.Rel(root, path)
				rel := kind + "/" + filepath.ToSlash(inside)
				warns = append(warns, rel+": unreadable, existing entries under it are kept")
				keep = append(keep, rel)
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if path != root && strings.HasPrefix(d.Name(), ".") { // .obsidian, .trash, .git ...
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".md") || strings.HasPrefix(d.Name(), ".") { // a symlink could point outside the repository
				return nil
			}
			inside, _ := filepath.Rel(root, path)
			rel := kind + "/" + filepath.ToSlash(inside)
			cands = append(cands, cand{id: strings.TrimSuffix(rel, ".md"), kind: kind, rel: rel, abs: path})
			return nil
		})
		if werr != nil {
			return nil, nil, nil, werr
		}
	}
	projects, perr := LoadProjects(repo)
	if perr != nil {
		return nil, nil, nil, perr
	}
	seenProject := map[string]bool{}
	seenID := map[string]string{}
	for _, pr := range projects {
		if pr.Name == "" || strings.ContainsAny(pr.Name, "/\\ ") || strings.HasPrefix(pr.Name, ".") {
			return nil, nil, nil, fmt.Errorf("projects.yaml: invalid project name %q (no slash, backslash, space or leading dot)", pr.Name)
		}
		if seenProject[pr.Name] {
			return nil, nil, nil, fmt.Errorf("projects.yaml: project %q is listed twice", pr.Name)
		}
		seenProject[pr.Name] = true
		ignore := map[string]bool{}
		for _, d := range pr.IgnoreDirs {
			ignore[d] = true
		}
		for _, rawRoot := range pr.CodeRoots {
			if strings.TrimSpace(rawRoot) == "" {
				return nil, nil, nil, fmt.Errorf("project %s: an empty code root would be the memory repository itself", pr.Name)
			}
			root, rerr := resolveRoot(repo, rawRoot)
			if rerr == nil {
				if rr, e2 := filepath.EvalSymlinks(repo); e2 == nil && badRoot(rr, root) {
					rerr = fmt.Errorf("the code root is the memory repository, contains it, or lies inside wiki/, notes/ or facts/")
				}
			}
			if rerr != nil {
				return nil, nil, nil, fmt.Errorf("project %s: code root %q: %w (refusing to rescan: that would remove the project's code entries)", pr.Name, rawRoot, rerr)
			}
			found := 0
			werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				rel, _ := filepath.Rel(root, path)
				rel = filepath.ToSlash(rel)
				if err != nil {
					warns = append(warns, "code/"+pr.Name+"/"+rel+": unreadable, existing entries under it are kept")
					keep = append(keep, "code/"+pr.Name+"/"+rel)
					if d != nil && d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					if rel != "." && (skipParts[d.Name()] || ignore[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() || skipCode(rel, ignore) {
					return nil
				}
				found++
				id := "code/" + pr.Name + "/" + rel
				if prev, dup := seenID[id]; dup {
					warns = append(warns, id+": also found under "+prev+"; the first code root wins")
					return nil
				}
				seenID[id] = root
				cands = append(cands, cand{id: id, kind: "code", project: pr.Name, rel: rel, abs: path})
				return nil
			})
			if werr != nil {
				return nil, nil, nil, fmt.Errorf("code root %s: %w", rawRoot, werr)
			}
			if found == 0 {
				return nil, nil, nil, fmt.Errorf("project %s: code root %q yielded no files (refusing to rescan: that would remove the project's code entries)", pr.Name, rawRoot)
			}
		}
	}
	return cands, warns, keep, nil
}

// keyBlockRe matches a private key block. A file holding one is never indexed.
var keyBlockRe = regexp.MustCompile("-----BEGIN [A-Z ]*PRIVATE" + " KEY( BLOCK)?-----|PuTTY-User-" + "Key-File") // split so the leak gate does not match its own source

// credAssignRe matches a credential-looking assignment. Source code is full of false positives (a name on the left,
// a function call on the right), so a hit is reported and the file is still indexed.
var credAssignRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|private_?key)[a-z0-9_]*\s*[:=]\s*["']?[A-Za-z0-9/+_=-]{12,}`)

// prepare decides whether a file is to be indexed and returns its content hash, without chunking it: a file whose
// hash has not changed is never chunked. ok is false for a binary file, an oversized or empty one, or one that
// holds a private key block.
func (c cand) prepare(raw []byte) (hash, warn string, ok bool) {
	if keyBlockRe.Match(raw) { // in any file, markdown included: what is indexed can be returned to an agent
		return "", c.id + ": holds a private key block, not indexed", false
	}
	if c.kind != "code" {
		return sum(raw), "", true
	}
	if isBinary(raw) || len(raw) > maxCodeBytes || len(strings.TrimSpace(string(raw))) == 0 {
		return "", "", false
	}
	if strings.HasSuffix(strings.ToLower(c.rel), ".json") && len(raw) > maxJSONBytes {
		return "", "", false
	}
	if credAssignRe.Match(raw) {
		warn = c.id + ": credential-like text (indexed; review the file)"
	}
	return sum(raw), warn, true
}

// parseCalls counts parse calls; a test uses it to show that an unchanged file is not chunked again.
var parseCalls atomic.Int64

// parse chunks a file that prepare accepted.
func (c cand) parse(raw []byte) (parsed, string) {
	parseCalls.Add(1)
	if c.kind != "code" {
		pw := parseMarkdown(c.kind, c.rel, raw)
		if pw.warn != "" {
			pw.warn = c.rel + ": " + pw.warn
		}
		return pw.parsed, pw.warn
	}
	return parsed{node: nodeRow{ID: c.id, Kind: "code", Project: c.project, Path: c.rel, Title: filepath.Base(c.rel),
		Status: "active", Hash: sum(raw)}, chunks: chunk.Code(string(raw))}, ""
}

type parsedWithWarn struct {
	parsed
	warn string
}

func parseMarkdown(kind, rel string, raw []byte) parsedWithWarn {
	doc := md.Parse(raw)
	segs := strings.Split(rel, "/")
	project := ""
	if len(segs) > 2 {
		project = segs[1]
	}
	id := strings.TrimSuffix(rel, ".md")
	title := doc.Str("title")
	if title == "" {
		for _, ln := range strings.Split(doc.Body, "\n") {
			if strings.HasPrefix(ln, "# ") {
				title = strings.TrimSpace(ln[2:])
				break
			}
		}
	}
	if title == "" {
		title = strings.TrimSuffix(segs[len(segs)-1], ".md")
	}
	status := doc.Str("status")
	if status == "" {
		status = "active" // a node with no status counts as active
	}
	nk := doc.Str("kind")
	if nk == "" && kind == "notes" {
		nk = "learning"
	}
	n := nodeRow{ID: id, Kind: strings.TrimSuffix(kind, "s"), Project: project, Path: rel, Title: title, Status: status,
		RetiredReason: doc.Str("retired_reason"), SupersededBy: doc.Str("superseded_by"), Pinned: doc.Bool("pinned"),
		NoteKind: nk, Created: doc.Str("created"), Updated: doc.Str("updated"), Hash: sum(raw)}
	var pieces []chunk.Piece
	if kind == "wiki" {
		pieces = chunk.Wiki(doc.Body)
	} else if strings.TrimSpace(doc.Body) != "" {
		pieces = []chunk.Piece{{Text: strings.TrimSpace(doc.Body)}}
	}
	var edges []edge
	for _, rel := range Relations {
		for _, v := range doc.List(rel) {
			edges = append(edges, edge{rel, v, "frontmatter"})
		}
	}
	for _, t := range md.WikiLinks(doc.Body) {
		edges = append(edges, edge{"links_to", t, "body"})
	}
	return parsedWithWarn{parsed{n, pieces, edges}, doc.Error}
}

// Rescan makes the index match the files under repo (and the code roots in projects.yaml).
// A file is read and chunked only when its content hash differs from the stored one; nodes whose files are gone
// are removed; nodes under an unreadable path are kept.
func (d *DB) Rescan(repo string) (Result, error) {
	var res Result
	cands, warns, keep, err := gather(repo)
	if err != nil {
		return res, err
	}
	res.Warnings = warns
	existing := map[string]string{}
	rows, err := d.Query(`SELECT id, content_hash FROM nodes`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return res, err
		}
		existing[id] = h
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()

	tx, err := d.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	remove := func(id string) error {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE rowid IN (SELECT id FROM chunks WHERE node_id=?)`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, id) // chunks and edges cascade
		return err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].id < cands[j].id })
	present := make(map[string]bool, len(cands))
	for _, c := range cands {
		if fi, err := os.Stat(c.abs); err == nil && c.kind == "code" && fi.Size() > maxCodeBytes {
			continue // too large to be source; treated as absent
		}
		raw, err := os.ReadFile(c.abs)
		if err != nil {
			res.Warnings = append(res.Warnings, c.id+": unreadable, existing entry is kept")
			present[c.id] = true
			continue
		}
		hash, warn, ok := c.prepare(raw)
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
		if !ok {
			continue
		}
		present[c.id] = true
		old, had := existing[c.id]
		if had && old == hash {
			res.Unchanged++
			continue
		}
		p, pwarn := c.parse(raw)
		if pwarn != "" {
			res.Warnings = append(res.Warnings, pwarn)
		}
		if len(p.chunks) == 0 && c.kind == "code" { // nothing left after chunking
			delete(present, c.id)
			continue
		}
		if had {
			if err := remove(c.id); err != nil {
				return res, err
			}
			res.Changed++
			res.ChangedIDs = append(res.ChangedIDs, c.id)
		} else {
			res.Added++
		}
		if err := insert(tx, p); err != nil {
			return res, fmt.Errorf("%s: %w", c.id, err)
		}
	}
	var gone []string
	for id := range existing {
		if present[id] {
			continue
		}
		kept := false
		for _, k := range keep {
			k = strings.TrimSuffix(k, ".md")
			if id == k || strings.HasPrefix(id, k+"/") {
				kept = true
				break
			}
		}
		if !kept {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		if err := remove(id); err != nil {
			return res, err
		}
		res.Removed++
	}
	return res, tx.Commit()
}

func insert(tx *sql.Tx, p parsed) error {
	n := p.node
	pin := 0
	if n.Pinned {
		pin = 1
	}
	if _, err := tx.Exec(`INSERT INTO nodes(id,kind,project,path,title,status,retired_reason,superseded_by,pinned,note_kind,created_at,updated_at,content_hash)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, n.ID, n.Kind, n.Project, n.Path, n.Title, n.Status, n.RetiredReason, n.SupersededBy, pin,
		n.NoteKind, n.Created, n.Updated, n.Hash); err != nil {
		return err
	}
	for i, c := range p.chunks {
		res, err := tx.Exec(`INSERT INTO chunks(node_id,seq,section,text,text_hash) VALUES(?,?,?,?,?)`, n.ID, i, c.Section, c.Text, sum([]byte(c.Text)))
		if err != nil {
			return err
		}
		rowid, _ := res.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO chunks_fts(rowid,text,meta) VALUES(?,?,?)`, rowid, c.Text, metaText(n)); err != nil {
			return err
		}
	}
	for _, e := range p.edges {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO edges(subject_id,relation,object_ref,source) VALUES(?,?,?,?)`, n.ID, e.Relation, e.Object, e.Source); err != nil {
			return err
		}
	}
	return nil
}

// isBinary reports whether data looks like a binary file: a NUL byte in the first 8 KB.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}

// metaText is what a title or path match is searched in: the title, then the path with its separators as spaces
// (a file can be found by its name or its directory).
func metaText(n nodeRow) string {
	return n.Title + " " + strings.NewReplacer("/", " ", ".", " ", "-", " ").Replace(n.Path)
}

// badRoot reports whether a code root would index the memory repository's own entries a second time: the root is the
// repository, contains it, or lies inside wiki/, notes/ or facts/.
func badRoot(repo, root string) bool {
	in := func(parent, child string) bool {
		r, err := filepath.Rel(parent, child)
		return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
	}
	if in(root, repo) {
		return true // the root is the repository or one of its parents
	}
	for _, layer := range []string{"wiki", "notes", "facts"} {
		if in(filepath.Join(repo, layer), root) {
			return true
		}
	}
	return false
}
