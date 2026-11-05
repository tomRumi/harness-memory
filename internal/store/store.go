// Package store holds the write paths of the memory repository: learn, fact_add, retire, feedback, wiki_read.
// Every write is a markdown file; the index is updated from that file, and one committer commits the files.
package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

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
	// EmbedTimeout bounds the embedding that follows a write. It runs after the write, in the background.
	EmbedTimeout = 5 * time.Second
)

// ErrHandWritten is returned by the index page writers when the project's Index.md is not the generated one.
var ErrHandWritten = errors.New("Index.md is hand-written; left alone")

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Store is the write side. Writes are serialised by a mutex, so concurrent callers queue instead of failing.
type Store struct {
	Repo   string
	DB     *index.DB
	Emb    index.Embedder // nil: no embedder configured (writes then store text and skip vectors)
	Commit *Committer
	// OnWrite, if set, is called after a write has been indexed and embedded (the server reloads its vectors there).
	OnWrite func()
	// Matrix, if set, returns the loaded vectors for the "Related" line of wiki_read (the server shares its own).
	// Without it the matrix is read from the index on each call when there is an embedder.
	Matrix func() *index.Matrix
	mu     sync.Mutex
	ev     *sql.DB
	// lockPath is the lock file shared by every process that writes this memory home.
	lockPath string

	embedMu   sync.Mutex
	embedCond *sync.Cond
	embedRun  bool // the embedding worker is running
	closed    bool // Close was called: no new embedding work starts
	embedWant bool // a write came in since the worker last started a pass
	embedWith index.Embedder
	embedErr  error // the last embedding error after a write
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
	s := &Store{Repo: repo, DB: db, Emb: emb, Commit: commit, ev: ev, lockPath: filepath.Join(filepath.Dir(eventsPath), "write.lock")}
	s.embedCond = sync.NewCond(&s.embedMu)
	return s, nil
}

// Close makes the last commit, stops the committer and closes the event history.
func (s *Store) Close() error {
	s.embedMu.Lock()
	s.closed = true // a write after Close starts no worker
	s.embedMu.Unlock()
	if s.Commit != nil {
		s.Commit.Stop() // the commit first: it does not wait for the embedder
	}
	s.WaitEmbed()
	return s.ev.Close()
}

// Event appends to events.db. Failures are ignored: history must never block a write.
func (s *Store) Event(kind, node, project, detail string) {
	s.ev.Exec(`INSERT INTO events(at,kind,node_id,project,detail) VALUES(?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339), kind, node, project, detail)
}

// DetID derives a stable id from the text, laid out like a UUID: sha1 over the parts joined by NUL
// so the same note always has the same id.
func DetID(prefix string, parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(append([]string{prefix}, parts...), "\x00")))
	x := fmt.Sprintf("%x", h)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// lockWrite takes the write lock: the mutex of this process, then the lock file of the memory home (so that another
// process, for example `hms curate -submit` beside `hms serve`, waits). The returned function releases both.
func (s *Store) lockWrite() (func(), error) {
	s.mu.Lock()
	unlock, err := lockFile(s.lockPath)
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("write lock: %w", err)
	}
	return func() { unlock(); s.mu.Unlock() }, nil
}

// Project normalises a project name the way every write does (trimmed, lower case; empty is the global project) and
// refuses a name that cannot be a directory.
func Project(p string) (string, error) { return project(p) }

// ValidProject is Project for callers that must not guess: a name with capitals is refused, not lower-cased.
func ValidProject(p string) (string, error) {
	p = strings.TrimSpace(p)
	if l, err := project(p); err != nil {
		return "", err
	} else if p != "" && l != p {
		return "", fmt.Errorf("project names are lower-case: %s", l)
	}
	return project(p)
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

// afterWrite updates the index for the changed files (a file that is gone leaves the index) and tells the committer.
// Embedding of what is new follows in the background (see kickEmbed): a model server that is down or hung never fails
// or delays a write and never keeps a saved note out of git. The error is kept (EmbedError) and recall uses keywords
// for the note until a vector exists.
func (s *Store) afterWrite(ctx context.Context, rels ...string) error {
	for _, r := range rels {
		if err := s.DB.Upsert(s.Repo, r); err != nil {
			return err
		}
	}
	s.finish(ctx, "", rels...)
	return nil
}

// finish tells the committer about files whose index entries are already up to date (msg: the commit message, empty
// for the default) and asks for embedding. It does no slow work, so it is safe under the write lock.
func (s *Store) finish(ctx context.Context, msg string, rels ...string) {
	if s.Commit != nil {
		s.Commit.NotifyMsg(msg, rels...)
	}
	s.kickEmbed()
}

// kickEmbed starts the embedding worker, or tells the running one to make another pass. There is one worker at a
// time. A pass has EmbedTimeout; OnWrite is called after each pass (and at once when there is no embedder).
func (s *Store) kickEmbed() {
	emb := s.Emb
	if emb == nil {
		if s.OnWrite != nil {
			s.OnWrite()
		}
		return
	}
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	if s.closed {
		return
	}
	s.embedWant, s.embedWith = true, emb
	if s.embedRun {
		return
	}
	s.embedRun = true
	go s.embedLoop()
}

func (s *Store) embedLoop() {
	for {
		s.embedMu.Lock()
		if !s.embedWant {
			s.embedRun = false
			s.embedCond.Broadcast()
			s.embedMu.Unlock()
			return
		}
		s.embedWant = false
		emb := s.embedWith
		s.embedMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), EmbedTimeout)
		_, err := s.DB.EmbedPending(ctx, emb, nil)
		if err == nil && ctx.Err() != nil {
			err = fmt.Errorf("embedding did not finish in %s: %w", EmbedTimeout, ctx.Err()) // EmbedPending stops quietly at the deadline
		}
		cancel()
		s.embedMu.Lock()
		s.embedErr = err
		if err != nil {
			s.embedWant = false // a failing embedder is not asked again by every write; the catch-up loop retries
		}
		s.embedMu.Unlock()
		if s.OnWrite != nil {
			s.OnWrite()
		}
	}
}

// WaitEmbedMax is the longest WaitEmbed (and so Close) waits for the embedding worker, however many writes arrive.
const WaitEmbedMax = 5 * time.Second

// WaitEmbed returns when no embedding is running, or after WaitEmbedMax at the latest. Tests use it; a write never
// waits for it.
func (s *Store) WaitEmbed() {
	s.embedMu.Lock()
	defer s.embedMu.Unlock()
	if !s.embedRun {
		return
	}
	t := time.AfterFunc(WaitEmbedMax, func() {
		s.embedMu.Lock()
		s.embedCond.Broadcast()
		s.embedMu.Unlock()
	})
	defer t.Stop()
	deadline := time.Now().Add(WaitEmbedMax)
	for s.embedRun && time.Now().Before(deadline) {
		s.embedCond.Wait()
	}
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
	if !hasLetterOrDigit(text) {
		return "", fmt.Errorf("nothing to remember: the text has no letter or digit")
	}
	if r := []rune(text); len(r) > MaxNote {
		text = string(r[:MaxNote-3]) + "…"
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "learning"
	}
	unlock, err := s.lockWrite()
	if err != nil {
		return "", err
	}
	defer unlock()
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

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
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
	unlock, err := s.lockWrite()
	if err != nil {
		return "", err
	}
	defer unlock()
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
	unlock, err := s.lockWrite()
	if err != nil {
		return "", err
	}
	defer unlock()
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
	rel, err := s.removeNode(id, p, "retire", reason)
	if err != nil {
		return "", err
	}
	if err := s.afterWrite(ctx, rel); err != nil {
		return "", err
	}
	out := "deleted " + shortID(id)
	if reason != "" {
		out += " (" + truncate(reason, 80) + ")"
	}
	return out, nil
}

// removeNode deletes the file of a note or fact and records the event (none when event is empty); the index and the committer are not told.
// The caller holds s.mu.
func (s *Store) removeNode(id, project, event, detail string) (rel string, err error) {
	rel = id + ".md"
	if err := os.Remove(filepath.Join(s.Repo, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if event != "" {
		s.Event(event, id, project, detail)
	}
	return rel, nil
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
// unhelpful and no session rated helpful. It only lists: `hms curate` shows them and `retire` deletes.
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
		if _, err := os.Stat(filepath.Join(s.Repo, filepath.FromSlash(id)+".md")); err != nil {
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
// followed by a footer naming the pages it links to, the pages that link to it and the pages closest in meaning.
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
	if r := s.relatedLine(id, proj); r != "" {
		b.WriteString(r)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n---\n" + b.String()
}

// WriteIndexPage regenerates wiki/<project>/Index.md from the pages in the index. The file is written (atomically)
// only when its content would change; the index is updated and the committer is told. It reports whether it wrote.
// An Index.md that the service did not generate (no `generated: true` in its frontmatter) is never overwritten: the
// error is ErrHandWritten.
func (s *Store) WriteIndexPage(ctx context.Context, proj string) (bool, error) {
	unlock, err := s.lockWrite()
	if err != nil {
		return false, err
	}
	defer unlock()
	rel, content, wrote, err := s.indexPageLocked(proj)
	if err != nil || !wrote {
		return false, err
	}
	if err := writeFile(filepath.Join(s.Repo, filepath.FromSlash(rel)), []byte(content)); err != nil {
		return false, err
	}
	if err := s.DB.Upsert(s.Repo, rel); err != nil {
		return false, err
	}
	s.finish(ctx, "", rel)
	return true, nil
}

// indexPageLocked builds the index page of a project and says whether it must be written: not when the file already
// has this content, and never over a hand-written Index.md. The caller holds the write lock.
func (s *Store) indexPageLocked(proj string) (rel, content string, wrote bool, err error) {
	if proj == "" || strings.ContainsAny(proj, "/\\") || strings.HasPrefix(proj, ".") {
		return "", "", false, fmt.Errorf("invalid project name %q", proj)
	}
	content, err = s.DB.BuildIndexPage(proj)
	if err != nil {
		return "", "", false, err
	}
	rel = "wiki/" + proj + "/" + wiki.IndexTitle + ".md"
	old, err := os.ReadFile(filepath.Join(s.Repo, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		if string(old) == content {
			return rel, content, false, nil
		}
		if !md.Parse(old).Bool("generated") {
			return rel, content, false, ErrHandWritten
		}
	case !os.IsNotExist(err): // a file that cannot be read is not known to be the generated one: it is left alone
		return rel, content, false, fmt.Errorf("%w (cannot read it: %v)", ErrHandWritten, err)
	}
	return rel, content, true, nil
}

// relatedLine is "Related: [[A]] (0.81), [[B]] (0.77)": the pages closest in meaning, at most index.RelatedShown with a
// cosine of at least m.RelatedMin(). It is empty when there are no vectors for the page or none that close. The
// embedder is never called: vectors already stored are used.
func (s *Store) relatedLine(id, proj string) string {
	if index.IsSpecialPage(id) {
		return ""
	}
	var m *index.Matrix
	if s.Matrix != nil {
		m = s.Matrix()
	} else if s.Emb != nil {
		m, _ = s.DB.LoadMatrix(s.Emb.ID())
	}
	rel, ok := m.RelatedPages(id, index.RelatedShown)
	if !ok {
		return ""
	}
	var names []string
	for _, r := range rel {
		if r.Score >= m.RelatedMin() {
			names = append(names, fmt.Sprintf("[[%s]] (%.2f)", linkName(r.NodeID, proj), r.Score))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "Related: " + strings.Join(names, ", ") + "\n"
}
