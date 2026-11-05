package store

import (
	"context"
	"crypto/sha1"
	"fmt"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// DetID derives a stable id from the text, laid out like a UUID: sha1 over the parts joined by NUL
// so the same note always has the same id.
func DetID(prefix string, parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(append([]string{prefix}, parts...), "\x00")))
	x := fmt.Sprintf("%x", h)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// Learn deposits one note and returns its id plus a short remark for the caller. It is idempotent: the same text
// returns the same id and changes nothing.
func (s *Store) Learn(ctx context.Context, text, proj, kind, supersedes string) (out string, err error) {
	proj, err = project(proj)
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
	id := DetID("mem", text)
	rel := "notes/" + proj + "/" + id + ".md"
	err = s.Do(func(tx *Tx) error {
		out = id
		if _, err := os.Stat(filepath.Join(s.Repo, rel)); err == nil {
			s.Event("learn_dup", id, proj, "")
			return nil
		}
		fm := map[string]any{"kind": kind, "created": time.Now().UTC().Format(time.RFC3339Nano), "source": "learn"}
		if err := tx.Write(rel, md.Render(fm, text)); err != nil {
			return err
		}
		note, err := s.supersede(tx, strings.TrimSpace(supersedes), proj, id)
		if err != nil {
			return err
		}
		out = id + note
		tx.Event("learn", id, proj, kind)
		tx.Finish(ctx, "")
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// supersede deletes the single note whose id starts with prefix (inside tx) and returns the remark for the caller.
func (s *Store) supersede(tx *Tx, prefix, proj, newID string) (string, error) {
	if prefix == "" {
		return "", nil
	}
	if len(prefix) < MinIDPrefix {
		return fmt.Sprintf(" || supersedes=%q is too short (give at least 4 characters of the id) — nothing was marked as replaced", prefix), nil
	}
	ids, err := s.DB.NodesByPrefix(prefix, proj)
	if err != nil {
		return "", err
	}
	var notes []string
	for _, n := range ids {
		if strings.HasPrefix(n, "notes/") && n != "notes/"+proj+"/"+newID {
			notes = append(notes, n)
		}
	}
	if len(notes) != 1 {
		return fmt.Sprintf(" || supersedes=%q matched no single note — nothing was marked as replaced", prefix), nil
	}
	if err := tx.Delete(notes[0], proj, "supersede", "->"+newID); err != nil {
		return "", err
	}
	return " || replaced " + ShortID(notes[0]) + " (deleted; git history keeps its text)", nil
}

func hasLetterOrDigit(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// ShortID is the first 8 characters of the last part of a node id or file name: the id recall prints.
func ShortID(nodeID string) string {
	b := filepath.Base(strings.TrimSuffix(nodeID, ".md"))
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
	id := DetID("fact", text)
	rel := "facts/" + proj + "/" + id + ".md"
	err = s.Do(func(tx *Tx) error {
		if _, err := os.Stat(filepath.Join(s.Repo, rel)); err == nil {
			return nil
		}
		fm := map[string]any{"category": category, "trust": trust, "created": time.Now().UTC().Format(time.RFC3339Nano)}
		if err := tx.Write(rel, md.Render(fm, text)); err != nil {
			return err
		}
		tx.Event("fact_add", id, proj, category)
		tx.Finish(ctx, "")
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// resolve finds the single note or fact whose id starts with prefix. ok is false and msg says why otherwise.
func (s *Store) resolve(prefix, proj string) (id string, msg string) {
	prefix = strings.TrimSpace(prefix)
	if len(prefix) < MinIDPrefix {
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
	proj, err := wiki.OptionalProject(proj)
	if err != nil {
		return "", err
	}
	out := ""
	err = s.Do(func(tx *Tx) error {
		id, msg := s.resolve(prefix, proj)
		if msg != "" {
			out = msg
			return nil
		}
		var p string
		if err := s.DB.QueryRow(`SELECT project FROM nodes WHERE id=?`, id).Scan(&p); err != nil {
			return err
		}
		reason = wiki.Cut(reason, 200)
		if err := tx.Delete(id, p, "retire", reason); err != nil {
			return err
		}
		tx.Finish(ctx, "")
		out = "deleted " + ShortID(id)
		if reason != "" {
			out += " (" + wiki.Cut(reason, 80) + ")"
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}
