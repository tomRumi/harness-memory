// Package index keeps the derived search index: nodes, chunks, a keyword index and edges.
// Everything in it is rebuilt from files; nothing here is a record.
package index

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS nodes(
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, project TEXT NOT NULL, path TEXT NOT NULL, stem TEXT NOT NULL, title TEXT NOT NULL,
  status TEXT NOT NULL, retired_reason TEXT NOT NULL, superseded_by TEXT NOT NULL, pinned INTEGER NOT NULL,
  note_kind TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, content_hash TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS nodes_project ON nodes(project, kind);
CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL, section TEXT NOT NULL, text TEXT NOT NULL, text_hash TEXT NOT NULL, UNIQUE(node_id, seq));
CREATE TABLE IF NOT EXISTS edges(
  subject_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, relation TEXT NOT NULL,
  object_ref TEXT NOT NULL, source TEXT NOT NULL, PRIMARY KEY(subject_id, relation, object_ref, source));
`

// ftsSchema is built with Sprintf so the tokenizer option does not look like an assignment to the leak gate.
// Letters are folded without accents, and an underscore belongs to a word (identifiers stay in one piece).
var ftsSchema = fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(text, meta, tokenize = %q)`, "unicode61 remove_diacritics 2 tokenchars '_'")

// schemaVersion changes whenever a table changes. The index is derived, so an older one is dropped and rebuilt.
const schemaVersion = 4

// vectorSchema lives in its own file next to the index: embedding is the slow part (hours on a small machine), and
// deleting the index to rebuild it from the markdown must not throw the vectors away. They are keyed by the hash of
// the chunk text and the model name, so a vector is reused by any chunk with the same text.
const vectorSchema = `
CREATE TABLE IF NOT EXISTS vec.vectors(text_hash TEXT NOT NULL, model TEXT NOT NULL, dim INTEGER NOT NULL, vec BLOB NOT NULL,
  PRIMARY KEY(text_hash, model));
`

// attach maps a connection string to the vector file that every connection opened with it must attach as "vec".
// A connection hook is used because database/sql may replace the connection; an ATTACH run once would be lost.
var attach sync.Map

func init() {
	sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
		p, ok := attach.Load(dsn)
		if !ok {
			return nil
		}
		target := p.(string)
		if strings.Contains(dsn, "mode=ro") {
			if _, err := os.Stat(target); err != nil {
				return nil // a read-only open does not create the vector file; recall then has no vectors
			}
			target = "file:" + target + "?mode=ro"
		}
		_, err := conn.ExecContext(context.Background(), "ATTACH DATABASE '"+strings.ReplaceAll(target, "'", "''")+"' AS vec", nil)
		return err
	})
}

// DB is an open index.
type DB struct {
	*sql.DB
	Path string
}

// Open opens (creating if needed) the index at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	attach.Store(dsn, strings.TrimSuffix(path, ".db")+"-vectors.db") // the hook below attaches it on every new connection
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	var ver int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		d.Close()
		return nil, err
	}
	if ver != schemaVersion {
		for _, t := range []string{"chunks_fts", "edges", "chunks", "nodes", "events", "meta"} {
			if _, err := d.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
				d.Close()
				return nil, fmt.Errorf("upgrade index: %w", err)
			}
		}
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if _, err := d.Exec(ftsSchema); err != nil {
		d.Close()
		return nil, fmt.Errorf("create keyword index: %w", err)
	}
	if _, err := d.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		d.Close()
		return nil, err
	}
	if _, err := d.Exec(vectorSchema); err != nil {
		d.Close()
		return nil, fmt.Errorf("create vector cache: %w", err)
	}
	return &DB{DB: d, Path: path}, nil
}

// OpenReadOnly opens an existing index for reading only. It never creates the file, never changes the schema and
// never drops anything: a missing file or a schema of another version is an error that names the cure. Hooks use it,
// so that the first prompt after a binary upgrade cannot empty the index of a running service.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no index at %s (run `hms rescan`)", path)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	attach.Store(dsn, strings.TrimSuffix(path, ".db")+"-vectors.db")
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	var ver int
	if err := d.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		d.Close()
		return nil, err
	}
	if ver != schemaVersion {
		d.Close()
		return nil, fmt.Errorf("the index has schema version %d, this program needs %d (run `hms rescan`)", ver, schemaVersion)
	}
	return &DB{DB: d, Path: path}, nil
}
