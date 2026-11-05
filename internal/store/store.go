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
	"github.com/tomRumi/harness-memory-service/internal/wiki"
	_ "modernc.org/sqlite"
)

const (
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
	// OnWrite, if set, is called after a write has been indexed and embedded (the server reloads its vectors there).
	OnWrite func()
	mu      sync.Mutex
	ev      *sql.DB

	embedMu  sync.Mutex
	embedErr error // the last embedding error after a write
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
	return &Store{Repo: repo, DB: db, Emb: emb, Commit: commit, ev: ev}, nil
}

// Close makes the last commit, stops the committer and closes the event history.
func (s *Store) Close() error {
	if s.Commit != nil {
		s.Commit.Stop()
	}
	return s.ev.Close()
}

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

// afterWrite updates the index for the changed files (a file that is gone leaves the index) and tells the committer;
// then it embeds what is new, best effort. A model server that is down never fails a write and never keeps a saved
// note out of git: the error is kept (EmbedError) and recall uses keywords for the note until a vector exists.
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
		_, err := s.DB.EmbedPending(ectx, s.Emb, nil)
		s.embedMu.Lock()
		s.embedErr = err
		s.embedMu.Unlock()
	}
	if s.OnWrite != nil {
		s.OnWrite()
	}
	return nil
}

// EmbedError is the error of the last embedding attempt after a write; nil when it worked or none was made.
func (s *Store) EmbedError() error {
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	return s.embedErr
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
	fm := map[string]any{"kind": kind, "created": time.Now().UTC().Format(time.RFC3339Nano), "source": "learn"}
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
				if err := os.Remove(filepath.Join(s.Repo, filepath.FromSlash(trel))); err != nil && !os.IsNotExist(err) {
					return id, err
				}
				changed = append(changed, trel)
				s.Event("supersede", notes[0], proj, "->"+id)
				note = " || replaced " + shortID(notes[0]) + " (deleted; git history keeps its text)"
			}
		}
	}
	for _, r := range changed {
		if err := s.DB.Upsert(s.Repo, r); err != nil {
			return id + note, err
		}
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
	fm := map[string]any{"category": category, "trust": trust, "created": time.Now().UTC().Format(time.RFC3339Nano)}
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

// Retire deletes a note or fact. The file is removed and the node leaves the index; the committer records the
// deletion, so git history keeps the text.
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
	var p string
	if err := s.DB.QueryRow(`SELECT project FROM nodes WHERE id=?`, id).Scan(&p); err != nil {
		return "", err
	}
	if r := []rune(reason); len(r) > 200 {
		reason = string(r[:200])
	}
	rel := id + ".md"
	if err := os.Remove(filepath.Join(s.Repo, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	s.Event("retire", id, p, reason)
	if err := s.afterWrite(ctx, rel); err != nil {
		return "", err
	}
	out := "deleted " + shortID(id)
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

// Feedback records whether a recalled note helped. Ratings go to events.db; they never remove a hit by themselves.
// RetireAfter is the number of distinct sessions that must rate a note unhelpful (and none helpful) before it is a
// candidate for retirement.
const RetireAfter = 5

// Feedback records a rating of a note or fact by one session. Only the last rating of a session counts, so one agent
// that repeats itself is one voice, not many.
func (s *Store) Feedback(prefix string, helpful bool, proj, session string) (string, error) {
	session = strings.TrimSpace(session)
	if session == "" {
		return "feedback needs a session (any stable name for this conversation or run) — nothing recorded", nil
	}
	if len(session) > 100 {
		session = session[:100]
	}
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
	s.Event(kind, id, p, session)
	h, u := s.Ratings(id)
	return fmt.Sprintf("recorded %s for %s (sessions: helpful %d, unhelpful %d)", kind, shortID(id), h, u), nil
}

// Ratings returns how many sessions rated a node helpful and unhelpful, by each session's last rating.
func (s *Store) Ratings(id string) (helpful, unhelpful int) {
	rows, err := s.ev.Query(`SELECT kind, detail FROM events WHERE node_id=? AND kind IN ('helpful','unhelpful') ORDER BY rowid`, id)
	if err != nil {
		return
	}
	defer rows.Close()
	last := map[string]string{}
	for rows.Next() {
		var k, d string
		rows.Scan(&k, &d)
		last[d] = k
	}
	for _, k := range last {
		if k == "helpful" {
			helpful++
		} else {
			unhelpful++
		}
	}
	return
}

// RetireCandidates lists notes (not facts, not wiki entries) that at least RetireAfter distinct sessions rated
// unhelpful and no session rated helpful. It only lists.
func (s *Store) RetireCandidates() []string {
	rows, err := s.ev.Query(`SELECT DISTINCT node_id FROM events WHERE kind='unhelpful' AND node_id LIKE 'notes/%'`)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []string
	for _, id := range ids {
		var n int
		s.DB.QueryRow(`SELECT count(*) FROM nodes WHERE id=?`, id).Scan(&n)
		if n == 0 {
			continue // already deleted
		}
		if h, u := s.Ratings(id); h == 0 && u >= RetireAfter {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// WikiRead returns a wiki page, given its path under wiki/ (project/Title.md, with or without the wiki/ prefix) or
// its title (Title or project/Title, any case, or an alias). A title is resolved the way a [[link]] is. The page is
// followed by a footer naming the pages it links to and the pages that link to it.
func (s *Store) WikiRead(path string) (string, error) {
	in := filepath.ToSlash(strings.TrimSpace(path))
	in = strings.TrimPrefix(in, "wiki/")
	if in == "" || strings.HasPrefix(in, "/") {
		return "", fmt.Errorf("give a path under wiki/ (project/Title.md) or a page title")
	}
	for _, seg := range strings.Split(in, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path leaves wiki/")
		}
	}
	if strings.HasSuffix(in, ".md") {
		text, err := s.readWikiFile(in)
		if err == nil {
			return text + s.linksFooter("wiki/"+strings.TrimSuffix(in, ".md")), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	name := strings.TrimSuffix(in, ".md")
	id, matches, err := s.DB.ResolveTitle(name)
	if err != nil {
		return "", err
	}
	switch {
	case id != "":
		text, err := s.readWikiFile(strings.TrimPrefix(id, "wiki/") + ".md")
		if err != nil {
			return "", fmt.Errorf("no wiki entry %q", path)
		}
		return text + s.linksFooter(id), nil
	case len(matches) > 1:
		return "", fmt.Errorf("several pages answer to %q: %s; give the project as project/Title", name, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("no wiki entry %q", path)
}

// readWikiFile reads wiki/<rel>, refusing a symlink that leads out of wiki/. A missing file is an os.IsNotExist error.
func (s *Store) readWikiFile(rel string) (string, error) {
	root := filepath.Join(s.Repo, "wiki")
	full := filepath.Join(root, filepath.FromSlash(rel))
	if r, err := filepath.EvalSymlinks(root); err == nil {
		if f, err := filepath.EvalSymlinks(full); err == nil {
			rl, err := filepath.Rel(r, f)
			if err != nil || strings.HasPrefix(rl, "..") {
				return "", fmt.Errorf("path leaves wiki/")
			}
		}
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// linkName is how a page is named in a footer: its file name, with the project in front when it is not in project.
func linkName(id, project string) string {
	rest := strings.TrimPrefix(id, "wiki/")
	proj, name, _ := strings.Cut(rest, "/")
	if proj == project {
		return filepath.Base(name)
	}
	return proj + "/" + filepath.Base(name)
}

// linksFooter returns the footer of wiki_read: "Links to: [[A]], [[B]]" and "Linked from: [[C]]", resolved links
// only, a line left out when it would be empty.
func (s *Store) linksFooter(id string) string {
	proj, _, _ := strings.Cut(strings.TrimPrefix(id, "wiki/"), "/")
	var b strings.Builder
	for _, l := range []struct {
		label string
		get   func(string) ([]string, error)
	}{{"Links to", s.DB.OutLinks}, {"Linked from", s.DB.Backlinks}} {
		ids, err := l.get(id)
		if err != nil || len(ids) == 0 {
			continue
		}
		names := make([]string, len(ids))
		for i, x := range ids {
			names[i] = "[[" + linkName(x, proj) + "]]"
		}
		b.WriteString(l.label + ": " + strings.Join(names, ", ") + "\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n---\n" + b.String()
}

// WriteIndexPage regenerates wiki/<project>/Index.md from the pages in the index. The file is written (atomically)
// only when its content would change; the index is updated and the committer is told. It reports whether it wrote.
func (s *Store) WriteIndexPage(ctx context.Context, proj string) (bool, error) {
	if proj == "" || strings.ContainsAny(proj, "/\\") || strings.HasPrefix(proj, ".") {
		return false, fmt.Errorf("invalid project name %q", proj)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	content, err := s.DB.BuildIndexPage(proj)
	if err != nil {
		return false, err
	}
	rel := "wiki/" + proj + "/" + wiki.IndexTitle + ".md"
	full := filepath.Join(s.Repo, filepath.FromSlash(rel))
	if old, err := os.ReadFile(full); err == nil && string(old) == content {
		return false, nil
	}
	if err := writeFile(full, []byte(content)); err != nil {
		return false, err
	}
	return true, s.afterWrite(ctx, rel)
}
