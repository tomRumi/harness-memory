// Package index keeps the derived search index: nodes, chunks, a keyword index and edges.
// Everything in it is rebuilt from files; nothing here is a record.
package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS nodes(
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, project TEXT NOT NULL, path TEXT NOT NULL, title TEXT NOT NULL,
  status TEXT NOT NULL, retired_reason TEXT NOT NULL, superseded_by TEXT NOT NULL, pinned INTEGER NOT NULL,
  note_kind TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, content_hash TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS nodes_project ON nodes(project, kind);
CREATE TABLE IF NOT EXISTS chunks(
  id INTEGER PRIMARY KEY, node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL, section TEXT NOT NULL, text TEXT NOT NULL, text_hash TEXT NOT NULL, UNIQUE(node_id, seq));
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(text, tokenize = "unicode61 remove_diacritics 2 tokenchars '_'");
CREATE TABLE IF NOT EXISTS edges(
  subject_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, relation TEXT NOT NULL,
  object_ref TEXT NOT NULL, source TEXT NOT NULL, PRIMARY KEY(subject_id, relation, object_ref, source));
CREATE TABLE IF NOT EXISTS events(at TEXT NOT NULL, kind TEXT NOT NULL, node_id TEXT NOT NULL, project TEXT NOT NULL, detail TEXT NOT NULL);
`

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
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &DB{DB: d, Path: path}, nil
}
