// Package store holds the write paths of the memory repository: learn, fact_add, retire, feedback, wiki_read.
// Every write is a markdown file; the index is updated from that file, and one committer commits the files.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
	_ "modernc.org/sqlite"
)

const (
	// QuietDelay is how long the committer waits after the last write before it commits.
	QuietDelay = 3 * time.Second
	// MinIDPrefix is the fewest characters of an id that name a note.
	MinIDPrefix = 4
	// MaxNote is the longest note kept in full; longer text is cut and ends with an ellipsis.
	MaxNote = 500
	// GlobalProject holds what belongs to no project.
	GlobalProject = "global"
	// EmbedTimeout bounds the embedding that follows a write. It runs after the write, in the background.
	EmbedTimeout = 5 * time.Second
)

// ErrHandWritten is returned by the index page writers when the project's Index.md is not the generated one.
var ErrHandWritten = errors.New("Index.md is hand-written; left alone")

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

// Sweep commits the notes, facts and pages that an earlier run left uncommitted or staged (see Committer.Sweep). It
// holds the write lock, so it never commits a multi-file operation of another process half way.
func (s *Store) Sweep() error {
	if s.Commit == nil {
		return nil
	}
	unlock, err := s.lockWrite()
	if err != nil {
		return err
	}
	defer unlock()
	s.Commit.Sweep()
	s.Commit.Flush()
	return nil
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

// project is the project of a write: the project rule (wiki.Project); no name means the global project.
func project(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return GlobalProject, nil
	}
	return wiki.Project(p)
}

// writeFile writes atomically: a reader never sees half a note.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), wiki.TempPrefix+"*")
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
