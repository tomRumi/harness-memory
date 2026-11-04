package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tomRumi/harness-memory-service/internal/chunk"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"go.yaml.in/yaml/v3"
)

// Relations that frontmatter may declare. Anything else in frontmatter is ignored by the edge extractor.
var Relations = []string{"supersedes", "depends_on", "part_of", "produced_by", "links_to"}

// Project is one entry of projects.yaml in the memory repository.
type Project struct {
	Name      string   `yaml:"name"`
	CodeRoots []string `yaml:"code_roots"`
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

func skipCode(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts[:len(parts)-1] {
		if skipParts[p] {
			return true
		}
	}
	base := parts[len(parts)-1]
	low := strings.ToLower(base)
	if skipBase[base] || strings.HasSuffix(low, ".min.js") || strings.HasSuffix(low, ".min.css") {
		return true
	}
	if skipSuffix[strings.ToLower(filepath.Ext(base))] {
		return true
	}
	lr := strings.ToLower(rel)
	if strings.HasSuffix(low, ".json") && (strings.Contains(lr, "cache") || strings.Contains(lr, "db/") || strings.Contains(lr, "data/")) {
		return true
	}
	return false
}

// collect parses every file the repository and the code roots currently hold, keyed by node id.
func collect(repo string) (map[string]parsed, []string, error) {
	out := map[string]parsed{}
	var warns []string
	for _, kind := range []string{"wiki", "notes", "facts"} {
		root := filepath.Join(repo, kind)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return filepath.SkipDir
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			rel, _ := filepath.Rel(repo, path)
			rel = filepath.ToSlash(rel)
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			p := parseMarkdown(kind, rel, raw)
			if w := p.warn; w != "" {
				warns = append(warns, rel+": "+w)
			}
			out[p.node.ID] = p.parsed
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	projects, err := LoadProjects(repo)
	if err != nil {
		return nil, nil, err
	}
	for _, pr := range projects {
		for _, root := range pr.CodeRoots {
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(root, path)
				rel = filepath.ToSlash(rel)
				if d.IsDir() {
					if skipParts[d.Name()] {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() || skipCode(rel) {
					return nil
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					return nil
				}
				pieces := chunk.Code(string(raw))
				if len(pieces) == 0 {
					return nil
				}
				id := "code/" + pr.Name + "/" + rel
				out[id] = parsed{node: nodeRow{ID: id, Kind: "code", Project: pr.Name, Path: rel, Title: filepath.Base(rel),
					Status: "active", Hash: sum(raw)}, chunks: pieces}
				return nil
			})
			if err != nil {
				return nil, nil, fmt.Errorf("code root %s: %w", root, err)
			}
		}
	}
	return out, warns, nil
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
// A node is re-read only when its content hash differs; nodes whose files are gone are removed.
func (d *DB) Rescan(repo string) (Result, error) {
	var res Result
	files, warns, err := collect(repo)
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
	ids := make([]string, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := files[id]
		old, had := existing[id]
		if had && old == p.node.Hash {
			res.Unchanged++
			continue
		}
		if had {
			if err := remove(id); err != nil {
				return res, err
			}
			res.Changed++
			res.ChangedIDs = append(res.ChangedIDs, id)
		} else {
			res.Added++
		}
		if err := insert(tx, p); err != nil {
			return res, fmt.Errorf("%s: %w", id, err)
		}
	}
	var gone []string
	for id := range existing {
		if _, ok := files[id]; !ok {
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
		if _, err := tx.Exec(`INSERT INTO chunks_fts(rowid,text) VALUES(?,?)`, rowid, c.Text); err != nil {
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
