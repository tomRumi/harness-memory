package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tomRumi/harness-memory/internal/md"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// Listing limits: a page holds DefaultListLimit notes unless asked otherwise, and never more than MaxListLimit.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ListNotes answers, as JSON, the notes of one project newest first (created, then id), with the count of all notes
// that match status ("" for any, Unverified or "active") and the page asked for. It reads the index for the order and
// the note files for the full text.
func (s *Store) ListNotes(proj, status string, limit, offset int) (string, error) {
	proj, err := wiki.Project(proj)
	if err != nil {
		return "", err
	}
	if status != "" && status != Unverified && status != "active" {
		return "", fmt.Errorf("status must be empty, %q or %q, not %q; nothing was listed", Unverified, "active", status)
	}
	if limit < 0 || offset < 0 {
		return "", fmt.Errorf("limit and offset may not be negative; nothing was listed")
	}
	if limit == 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	where, args := `kind='note' AND project=?`, []any{proj}
	switch status {
	case Unverified:
		where += ` AND status=?`
		args = append(args, Unverified)
	case "active":
		where += ` AND status<>?`
		args = append(args, Unverified)
	}
	type note struct {
		ID      string `json:"id"`
		Short   string `json:"short"`
		Kind    string `json:"kind"`
		Status  string `json:"status"`
		Author  string `json:"author"`
		Created string `json:"created"`
		Text    string `json:"text"`
	}
	out := struct {
		Project string `json:"project"`
		Total   int    `json:"total"`
		Notes   []note `json:"notes"`
	}{Project: proj, Notes: []note{}}
	if err := s.DB.QueryRow(`SELECT count(*) FROM nodes WHERE `+where, args...).Scan(&out.Total); err != nil {
		return "", err
	}
	rows, err := s.DB.Query(`SELECT id, path, note_kind, status, author, created_at FROM nodes WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var n note
		var path string
		if err := rows.Scan(&n.ID, &path, &n.Kind, &n.Status, &n.Author, &n.Created); err != nil {
			return "", err
		}
		if n.Status == "" {
			n.Status = "active"
		}
		n.Short = ShortID(n.ID)
		raw, err := os.ReadFile(filepath.Join(s.Repo, filepath.FromSlash(path)))
		if err != nil {
			return "", fmt.Errorf("cannot read note %s: %w; nothing was listed", n.Short, err)
		}
		n.Text = strings.TrimSpace(md.Parse(raw).Body)
		out.Notes = append(out.Notes, n)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	return string(b), err
}
