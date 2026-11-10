package store

import (
	"fmt"
	"github.com/tomRumi/harness-memory/internal/wiki"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Event appends to events.db. Failures are ignored: history must never block a write.
func (s *Store) Event(kind, node, project, detail string) {
	s.ev.Exec(`INSERT INTO events(at,kind,node_id,project,detail) VALUES(?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339), kind, node, project, detail)
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
	proj, err := wiki.OptionalProject(proj)
	if err != nil {
		return "", err
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
	return fmt.Sprintf("recorded %s for %s (sessions: helpful %d, unhelpful %d)", kind, ShortID(id), h, u), nil
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
