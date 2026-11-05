package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/md"
)

// NoteText returns the text of a note (its file without the frontmatter). id is the node id, notes/<project>/<id>.
func (s *Store) NoteText(id string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Repo, filepath.FromSlash(id)+".md"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(md.Parse(b).Body), nil
}

// AppendSection adds a section at the end of a wiki entry and marks the given notes as superseded by it, all under one
// lock and in one commit. When the entry does not exist yet it is created from header. Nothing is rewritten and no
// note is deleted: the notes stay on disk with status superseded and superseded_by set, and git keeps the history.
// wikiRel is the path of the entry below the repository, for example wiki/project/modules/rag.md.
func (s *Store) AppendSection(ctx context.Context, wikiRel, header, section string, noteIDs []string) error {
	wikiRel = filepath.ToSlash(wikiRel)
	if !strings.HasPrefix(wikiRel, "wiki/") || strings.Contains(wikiRel, "..") || !strings.HasSuffix(wikiRel, ".md") {
		return fmt.Errorf("not a wiki path: %q", wikiRel)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.health(ctx); err != nil {
		return err
	}
	full := filepath.Join(s.Repo, filepath.FromSlash(wikiRel))
	cur, err := os.ReadFile(full)
	switch {
	case err == nil:
		cur = []byte(strings.TrimRight(string(cur), "\n") + "\n\n")
	case os.IsNotExist(err):
		cur = []byte(header)
	default:
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := writeFile(full, append(cur, []byte(strings.TrimRight(section, "\n")+"\n")...)); err != nil {
		return err
	}
	entryID := strings.TrimSuffix(wikiRel, ".md")
	changed := []string{wikiRel}
	for _, id := range noteIDs {
		rel := id + ".md"
		if err := s.setFrontmatter(rel, map[string]any{"status": "superseded", "superseded_by": entryID}); err != nil {
			return err
		}
		changed = append(changed, rel)
		s.Event("supersede", id, "", "->"+entryID)
	}
	s.Event("curate", entryID, "", fmt.Sprintf("%d note(s) appended", len(noteIDs)))
	return s.afterWrite(ctx, changed...)
}

// Restore brings back a note that was retired, archived or superseded. Ratings given before the restore no longer count.
func (s *Store) Restore(ctx context.Context, prefix, proj string) (string, error) {
	if proj != "" {
		var err error
		if proj, err = project(proj); err != nil {
			return "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, msg := s.resolveAny(prefix, proj)
	if msg != "" {
		return msg, nil
	}
	var status, p string
	if err := s.DB.QueryRow(`SELECT status, project FROM nodes WHERE id=?`, id).Scan(&status, &p); err != nil {
		return "", err
	}
	if status == "active" {
		return shortID(id) + " is already active — nothing changed", nil
	}
	rel := id + ".md"
	if err := s.setFrontmatter(rel, map[string]any{"status": "active", "retired_reason": "", "superseded_by": ""}); err != nil {
		return "", err
	}
	s.Event("restore", id, p, status+" -> active")
	if err := s.afterWrite(ctx, rel); err != nil {
		return "", err
	}
	return "restored " + shortID(id) + " (was " + status + ")", nil
}

// resolveAny is resolve for notes and facts in any status.
func (s *Store) resolveAny(prefix, proj string) (string, string) {
	return s.resolve(prefix, proj)
}

// stamp is the date used in the headings of appended sections.
func Stamp() string { return time.Now().UTC().Format("2006-01-02") }
