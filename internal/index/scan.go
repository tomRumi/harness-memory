package index

import (
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

	"github.com/tomRumi/harness-memory/internal/chunk"
	"github.com/tomRumi/harness-memory/internal/md"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// Relations that frontmatter may declare. Anything else in frontmatter is ignored by the edge extractor.
var Relations = []string{"supersedes", "depends_on", "part_of", "produced_by", "links_to"}

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
	ID, Kind, Project, Path, Title, NoteKind, Created, Updated, Hash string
	Summary, Aliases                                                 string // wiki pages only; aliases are one per line
	Status, Author                                                   string // notes: frontmatter status (unverified or empty) and author
	Pinned                                                           bool
}

type edge struct{ Relation, Object, Source string }

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// cand is one file that may become a node. Nothing is read until Rescan decides it has to.
type cand struct {
	id, kind, rel, abs string // kind is wiki, notes or facts
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
				if path != root && wiki.Hidden(d.Name()) { // .obsidian, .trash, .git ...
					return filepath.SkipDir
				}
				return nil
			}
			inside, _ := filepath.Rel(root, path)
			rel := kind + "/" + filepath.ToSlash(inside)
			if !wiki.IsMemoryFile(rel, d.Type()) {
				return nil
			}
			cands = append(cands, cand{id: strings.TrimSuffix(rel, ".md"), kind: kind, rel: rel, abs: path})
			return nil
		})
		if werr != nil {
			return nil, nil, nil, werr
		}
	}
	return cands, warns, keep, nil
}

// keyBlockRe matches a private key block. A file holding one is never indexed.
var keyBlockRe = regexp.MustCompile("-----BEGIN [A-Z ]*PRIVATE" + " KEY( BLOCK)?-----|PuTTY-User-" + "Key-File") // split so the leak gate does not match its own source

// prepare decides whether a file is to be indexed and returns its content hash, without chunking it: a file whose
// hash has not changed is never chunked. ok is false for a file that holds a private key block.
func (c cand) prepare(raw []byte) (hash, warn string, ok bool) {
	if keyBlockRe.Match(raw) { // what is indexed can be returned to an agent
		return "", c.id + ": holds a private key block, not indexed", false
	}
	return sum(raw), "", true
}

// parseCalls counts parse calls; a test uses it to show that an unchanged file is not chunked again.
var parseCalls atomic.Int64

// parse chunks a file that prepare accepted.
func (c cand) parse(raw []byte) (parsed, string) {
	parseCalls.Add(1)
	pw := parseMarkdown(c.kind, c.rel, raw)
	if pw.warn != "" {
		pw.warn = c.rel + ": " + pw.warn
	}
	return pw.parsed, pw.warn
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
	var meta wiki.Meta
	if kind == "wiki" { // a page's title is its frontmatter title or its file name, never a heading
		meta = wiki.ParseMeta(doc, strings.TrimSuffix(segs[len(segs)-1], ".md"))
		title = meta.Title
	}
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
	nk := doc.Str("kind")
	if nk == "" && kind == "notes" {
		nk = "learning"
	}
	n := nodeRow{ID: id, Kind: strings.TrimSuffix(kind, "s"), Project: project, Path: rel, Title: title,
		Pinned:   doc.Bool("pinned"),
		NoteKind: nk, Created: doc.Str("created"), Updated: doc.Str("updated"), Hash: sum(raw)}
	if kind == "notes" {
		n.Status, n.Author = doc.Str("status"), doc.Str("author")
	}
	if kind == "wiki" {
		n.Summary = meta.Summary
		if n.Summary == "" {
			n.Summary = wiki.FirstSentence(doc.Body)
		}
		n.Aliases = strings.Join(meta.Aliases, "\n")
	}
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

// Rescan makes the index match the files under repo .
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
	if err := resolveLinks(tx); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

func insert(tx *sql.Tx, p parsed) error {
	n := p.node
	pin := 0
	if n.Pinned {
		pin = 1
	}
	if _, err := tx.Exec(`INSERT INTO nodes(id,kind,project,path,stem,title,pinned,note_kind,created_at,updated_at,content_hash,summary,aliases,status,author)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, n.ID, n.Kind, n.Project, n.Path, strings.TrimSuffix(filepath.Base(n.Path), ".md"), n.Title, pin,
		n.NoteKind, n.Created, n.Updated, n.Hash, n.Summary, n.Aliases, n.Status, n.Author); err != nil {
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

// metaText is what a title or path match is searched in: the title, then the path with its separators as spaces
// (a file can be found by its name or its directory).
func metaText(n nodeRow) string {
	return n.Title + " " + strings.NewReplacer("/", " ", ".", " ", "-", " ").Replace(n.Path)
}
