package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Upsert re-reads one markdown file of the repository (rel is its path under the repository, for example
// notes/p/<id>.md) and makes the index match it; a file that no longer exists is removed. It is the fast path used after
// a write, where a full rescan would read every file.
func (d *DB) Upsert(repo, rel string) error {
	rel = filepath.ToSlash(rel)
	kind := strings.SplitN(rel, "/", 2)[0]
	if (kind != "wiki" && kind != "notes" && kind != "facts") || !strings.HasSuffix(rel, ".md") {
		return fmt.Errorf("not an indexable memory file: %s", rel)
	}
	id := strings.TrimSuffix(rel, ".md")
	raw, err := os.ReadFile(filepath.Join(repo, rel))
	tx, terr := d.Begin()
	if terr != nil {
		return terr
	}
	defer tx.Rollback()
	remove := func() error {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE rowid IN (SELECT id FROM chunks WHERE node_id=?)`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, id)
		return err
	}
	if os.IsNotExist(err) {
		if err := remove(); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	c := cand{id: id, kind: kind, rel: rel, abs: filepath.Join(repo, rel)}
	hash, _, ok := c.prepare(raw)
	if !ok {
		if err := remove(); err != nil {
			return err
		}
		return tx.Commit()
	}
	var old string
	if err := tx.QueryRow(`SELECT content_hash FROM nodes WHERE id=?`, id).Scan(&old); err == nil && old == hash {
		return tx.Commit()
	}
	if err := remove(); err != nil {
		return err
	}
	p, _ := c.parse(raw)
	if err := insert(tx, p); err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	return tx.Commit()
}

// NodesByPrefix returns the ids of active-or-not note and fact nodes whose file name starts with prefix, for
// resolving the short id that recall prints.
func (d *DB) NodesByPrefix(prefix, project string) ([]string, error) {
	q := `SELECT id FROM nodes WHERE kind IN ('note','fact') AND stem LIKE ? ESCAPE '\'`
	args := []any{strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix) + "%"}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	rows, err := d.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
