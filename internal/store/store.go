// Package store holds the write paths of the memory repository: learn, fact_add, retire, feedback, wiki_read.
// Every write is a markdown file; the index is updated from that file, and one committer commits the files.
package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/md"
	_ "modernc.org/sqlite"
)

const (
	// MemoryCap is how many active notes one project may hold before the oldest are archived (kept, not returned).
	MemoryCap = 50
	// MaxNote is the longest note kept in full; longer text is cut and ends with an ellipsis.
	MaxNote = 500
	// GlobalProject holds what belongs to no project.
	GlobalProject = "global"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Store is the write side. Writes are serialised by a mutex, so concurrent callers queue instead of failing.
type Store struct {
	Repo   string
	DB     *index.DB
	Emb    index.Embedder // nil: no embedder configured (writes then store text and skip vectors)
	Commit *Committer
	Cap    int
	// OnWrite, if set, is called after a write has been indexed and embedded (the server reloads its vectors there).
	OnWrite func()
	mu      sync.Mutex
	ev      *sql.DB
}

// New opens the store. events.db is the history of what was recalled, learned, rated and retired; it is not derived
// from the markdown and is never rebuilt, so it lives in its own file.
func New(repo string, db *index.DB, emb index.Embedder, commit *Committer, eventsPath string) (*Store, error) {
	ev, err := sql.Open("sqlite", "file:"+eventsPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	ev.SetMaxOpenConns(1)
	if _, err := ev.Exec(`CREATE TABLE IF NOT EXISTS events(at TEXT NOT NULL, kind TEXT NOT NULL, node_id TEXT NOT NULL, project TEXT NOT NULL, detail TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	return &Store{Repo: repo, DB: db, Emb: emb, Commit: commit, Cap: MemoryCap, ev: ev}, nil
}

func (s *Store) Close() error { return s.ev.Close() }

// Event appends to events.db. Failures are ignored: history must never block a write.
func (s *Store) Event(kind, node, project, detail string) {
	s.ev.Exec(`INSERT INTO events(at,kind,node_id,project,detail) VALUES(?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339), kind, node, project, detail)
}

// DetID derives a stable id from the text, laid out like a UUID: sha1 over the parts joined by NUL
// (the predecessor's scheme, server.py:306-308), so the same note always has the same id.
func DetID(prefix string, parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(append([]string{prefix}, parts...), "\x00")))
	x := fmt.Sprintf("%x", h)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

func project(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return GlobalProject, nil
	}
	if !slugRe.MatchString(p) {
		return "", fmt.Errorf("invalid project name %q (lower-case letters, digits, dot, dash, underscore)", p)
	}
	return p, nil
}

func (s *Store) health(ctx context.Context) error {
	if s.Emb == nil {
		return nil
	}
	hc, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.Emb.Query(hc, "healthcheck"); err != nil {
		return fmt.Errorf("embedder unavailable (%v): nothing was stored", err)
	}
	return nil
}

// writeFile writes atomically: a reader never sees half a note.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// afterWrite updates the index for the changed files and tells the committer; then it embeds what is new. The commit is
// scheduled before the embedding is tried, so a model server that is down cannot keep a saved note out of git.
func (s *Store) afterWrite(ctx context.Context, rels ...string) error {
	for _, r := range rels {
		if err := s.DB.Upsert(s.Repo, r); err != nil {
			return err
		}
	}
	if s.Commit != nil {
		s.Commit.Notify(rels...)
	}
	if s.Emb != nil {
		ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := s.DB.EmbedPending(ectx, s.Emb, nil); err != nil {
			return fmt.Errorf("saved, but embedding failed (run `hms embed`): %w", err)
		}
	}
	if s.OnWrite != nil {
		s.OnWrite()
	}
	return nil
}

// setFrontmatter changes keys of a file's frontmatter and leaves every other byte of the file as it was.
func (s *Store) setFrontmatter(rel string, set map[string]any) error {
	path := filepath.Join(s.Repo, rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := md.SetKeys(raw, set)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := writeFile(path, out); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// Learn deposits one note and returns its id plus a short remark for the caller. It is idempotent: the same text
// returns the same id and changes nothing.
func (s *Store) Learn(ctx context.Context, text, proj, kind, supersedes string) (string, error) {
	proj, err := project(proj)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("refusing to store an empty learning")
	}
	if r := []rune(text); len(r) > MaxNote {
		text = string(r[:MaxNote-3]) + "…"
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "learning"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := DetID("mem", text)
	rel := "notes/" + proj + "/" + id + ".md"
	if _, err := os.Stat(filepath.Join(s.Repo, rel)); err == nil {
		s.Event("learn_dup", id, proj, "")
		return id, nil
	}
	if err := s.health(ctx); err != nil {
		return "", err
	}
	fm := map[string]any{"kind": kind, "status": "active", "created": time.Now().UTC().Format(time.RFC3339Nano), "source": "learn"}
	if err := writeFile(filepath.Join(s.Repo, rel), md.Render(fm, text)); err != nil {
		return "", err
	}
	changed := []string{rel}
	note := ""
	if supersedes = strings.TrimSpace(supersedes); supersedes != "" {
		if len(supersedes) < 4 {
			note = fmt.Sprintf(" || supersedes=%q is too short (give at least 4 characters of the id) — nothing was marked as replaced", supersedes)
		} else {
			ids, err := s.DB.NodesByPrefix(supersedes, proj)
			if err != nil {
				return id, err
			}
			var notes []string
			for _, n := range ids {
				if strings.HasPrefix(n, "notes/") && n != strings.TrimSuffix(rel, ".md") {
					notes = append(notes, n)
				}
			}
			if len(notes) != 1 {
				note = fmt.Sprintf(" || supersedes=%q matched no single note — nothing was marked as replaced", supersedes)
			} else {
				trel := notes[0] + ".md"
				if err := s.setFrontmatter(trel, map[string]any{"status": "superseded", "superseded_by": id}); err != nil {
					return id, err
				}
				changed = append(changed, trel)
				s.Event("supersede", notes[0], proj, "->"+id)
				note = " || replaced " + shortID(notes[0])
			}
		}
	}
	// the index must show the new note and the superseded one before the cap is counted
	for _, r := range changed {
		if err := s.DB.Upsert(s.Repo, r); err != nil {
			return id + note, err
		}
	}
	archived, err := s.enforceCap(proj)
	if err != nil {
		return id, err
	}
	if len(archived) > 0 {
		changed = append(changed, archived...)
		s.Event("archive", id, proj, fmt.Sprintf("%d oldest note(s) archived", len(archived)))
		note += fmt.Sprintf(" || %d oldest note(s) archived (kept, no longer returned)", len(archived))
	}
	s.Event("learn", id, proj, kind)
	if err := s.afterWrite(ctx, changed...); err != nil {
		return id + note, err
	}
	return id + note, nil
}

func shortID(nodeID string) string {
	b := filepath.Base(nodeID)
	if len(b) > 8 {
		return b[:8]
	}
	return b
}

// enforceCap archives the oldest active notes of a project beyond Cap. It returns the files it changed.
func (s *Store) enforceCap(proj string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT id, created_at FROM nodes WHERE kind='note' AND project=? AND status='active'`, proj)
	if err != nil {
		return nil, err
	}
	type n struct{ id, created string }
	var live []n
	for rows.Next() {
		var x n
		if err := rows.Scan(&x.id, &x.created); err != nil {
			rows.Close()
			return nil, err
		}
		live = append(live, x)
	}
	rows.Close()
	if len(live) <= s.Cap {
		return nil, nil
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].created != live[j].created {
			return live[i].created < live[j].created
		}
		return live[i].id < live[j].id
	})
	over := len(live) - s.Cap
	var changed []string
	for _, x := range live[:over] {
		rel := x.id + ".md"
		if err := s.setFrontmatter(rel, map[string]any{"status": "archived"}); err != nil {
			return changed, err
		}
		changed = append(changed, rel)
	}
	return changed, nil
}

// AddFact stores a durable fact in facts/<project>/<id>.md.
func (s *Store) AddFact(ctx context.Context, text, proj, category string, trust float64) (string, error) {
	proj, err := project(proj)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("refusing to store an empty fact")
	}
	if category == "" {
		category = "general"
	}
	if trust <= 0 || trust > 1 {
		trust = 0.5
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := DetID("fact", text)
	rel := "facts/" + proj + "/" + id + ".md"
	if _, err := os.Stat(filepath.Join(s.Repo, rel)); err == nil {
		return id, nil
	}
	if err := s.health(ctx); err != nil {
		return "", err
	}
	fm := map[string]any{"category": category, "trust": trust, "status": "active", "created": time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeFile(filepath.Join(s.Repo, rel), md.Render(fm, text)); err != nil {
		return "", err
	}
	s.Event("fact_add", id, proj, category)
	return id, s.afterWrite(ctx, rel)
}

// resolve finds the single note or fact whose id starts with prefix. ok is false and msg says why otherwise.
func (s *Store) resolve(prefix, proj string) (id string, msg string) {
	prefix = strings.TrimSpace(prefix)
	if len(prefix) < 4 {
		return "", fmt.Sprintf("no single memory note matches %q (give at least 4 characters of the id recall prints) — nothing changed", prefix)
	}
	ids, err := s.DB.NodesByPrefix(prefix, proj)
	if err != nil || len(ids) != 1 {
		return "", fmt.Sprintf("no single memory note matches %q — nothing changed. Call recall first and use the id it prints.", prefix)
	}
	return ids[0], ""
}

// Retire withdraws a note or fact that nothing replaces. The file stays; recall stops returning it.
func (s *Store) Retire(ctx context.Context, prefix, reason, proj string) (string, error) {
	if proj != "" {
		var err error
		if proj, err = project(proj); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, msg := s.resolve(prefix, proj)
	if msg != "" {
		return msg, nil
	}
	var status, p string
	if err := s.DB.QueryRow(`SELECT status, project FROM nodes WHERE id=?`, id).Scan(&status, &p); err != nil {
		return "", err
	}
	if status != "active" {
		return fmt.Sprintf("%s is already %s — nothing changed", shortID(id), status), nil
	}
	if r := []rune(reason); len(r) > 200 {
		reason = string(r[:200])
	}
	rel := id + ".md"
	if err := s.setFrontmatter(rel, map[string]any{"status": "retired", "retired_reason": reason}); err != nil {
		return "", err
	}
	s.Event("retire", id, p, reason)
	if err := s.afterWrite(ctx, rel); err != nil {
		return "", err
	}
	out := "retired " + shortID(id)
	if reason != "" {
		out += " (" + truncate(reason, 80) + ")"
	}
	return out, nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Feedback records whether a recalled note helped. Ratings go to events.db; they shape recall order and the weekly
// report (step 7), and never remove a hit by themselves.
func (s *Store) Feedback(prefix string, helpful bool, proj string) (string, error) {
	if proj != "" {
		var err error
		if proj, err = project(proj); err != nil {
			return "", err
		}
	}
	id, msg := s.resolve(prefix, proj)
	if msg != "" {
		return msg, nil
	}
	kind := "unhelpful"
	if helpful {
		kind = "helpful"
	}
	var p string
	s.DB.QueryRow(`SELECT project FROM nodes WHERE id=?`, id).Scan(&p)
	s.Event(kind, id, p, "")
	h, u := s.Ratings(id)
	return fmt.Sprintf("recorded %s for %s (helpful %d, unhelpful %d)", kind, shortID(id), h, u), nil
}

// Ratings returns how often a node was rated helpful and unhelpful.
func (s *Store) Ratings(id string) (helpful, unhelpful int) {
	rows, err := s.ev.Query(`SELECT kind, count(*) FROM events WHERE node_id=? AND kind IN ('helpful','unhelpful') GROUP BY kind`, id)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		if k == "helpful" {
			helpful = n
		} else {
			unhelpful = n
		}
	}
	return
}

// WikiRead returns a wiki entry by its path under wiki/, for example project/modules/rag.md or wiki/project/page.md.
func (s *Store) WikiRead(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	path = strings.TrimPrefix(path, "wiki/")
	if path == "" || strings.Contains(path, "..") || strings.HasPrefix(path, "/") || !strings.HasSuffix(path, ".md") {
		return "", fmt.Errorf("give a path under wiki/ ending in .md, for example project/page.md")
	}
	root := filepath.Join(s.Repo, "wiki")
	full := filepath.Join(root, filepath.FromSlash(path))
	if r, err := filepath.EvalSymlinks(root); err == nil {
		if f, err := filepath.EvalSymlinks(full); err == nil {
			rel, err := filepath.Rel(r, f)
			if err != nil || strings.HasPrefix(rel, "..") {
				return "", fmt.Errorf("path leaves wiki/")
			}
		}
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("no wiki entry %q", path)
	}
	return string(b), nil
}
