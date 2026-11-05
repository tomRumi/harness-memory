package curate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/store"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

const (
	// DefaultMax is the number of notes of one project taken by one run.
	DefaultMax = 40
	// MaxRelated is the number of related pages handed to the model.
	MaxRelated = 5
	// MaxRelatedBody is the largest page (bytes) that is handed over whole as a related page.
	MaxRelatedBody = 40000
	// queryNote is how much of each note goes into the query that finds related pages.
	queryNote = 300
	// UnsortedTitle is the page that takes the notes a merge could not place.
	UnsortedTitle = "Unsorted"
)

// PageRef is one existing page: its title, one-line summary and path under the repository.
type PageRef struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Path    string `json:"path"`
}

// Related is an existing page handed over whole. Hash is the sha256 of the file bytes; Submit refuses to overwrite
// the page when the file has changed since.
type Related struct {
	Title string `json:"title"`
	Hash  string `json:"hash"`
	Body  string `json:"body"`
}

// Batch is what one project needs to be merged.
type Batch struct {
	Project string    `json:"project"`
	Notes   []Note    `json:"notes"`
	Pages   []PageRef `json:"pages"`
	Related []Related `json:"related"`
	Prompt  string    `json:"prompt,omitempty"`
}

// Hash is the hash of file bytes used for Related.Hash and Page.BaseHash.
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type waitingNote struct {
	Note
	created time.Time
}

// readNotes lists the notes of a project with a text, oldest first (frontmatter created, then id).
func readNotes(repo, project string) ([]waitingNote, error) {
	dir := filepath.Join(repo, "notes", project)
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []waitingNote
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		d := md.Parse(raw)
		text := strings.TrimSpace(d.Body)
		if text == "" {
			continue
		}
		created := time.Time{}
		if t, err := time.Parse(time.RFC3339Nano, d.Str("created")); err == nil {
			created = t
		} else if fi, err := e.Info(); err == nil {
			created = fi.ModTime()
		}
		out = append(out, waitingNote{Note{strings.TrimSuffix(name, ".md"), text}, created})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.Before(out[j].created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// noteProjects lists the projects that have a notes directory, sorted.
func noteProjects(repo string) []string {
	ents, _ := os.ReadDir(filepath.Join(repo, "notes"))
	var out []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Waiting is the number of notes a project has waiting and the age of the oldest.
type Waiting struct {
	Project string
	Notes   int
	Oldest  time.Duration
}

// WaitingNotes counts the waiting notes per project (projects without one are left out). It reads files only.
func WaitingNotes(repo string) ([]Waiting, error) {
	var out []Waiting
	for _, p := range noteProjects(repo) {
		ns, err := readNotes(repo, p)
		if err != nil {
			return nil, err
		}
		if len(ns) > 0 {
			out = append(out, Waiting{p, len(ns), time.Since(ns[0].created)})
		}
	}
	return out, nil
}

// pageFiles lists the pages of a project (any depth under wiki/<project>/), Index left out, sorted by title.
func pageFiles(repo, project string) ([]PageRef, error) {
	root := filepath.Join(repo, "wiki", project)
	var out []PageRef
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		name := e.Name()
		if e.IsDir() {
			if p != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			return nil
		}
		title := strings.TrimSuffix(name, ".md")
		if strings.EqualFold(title, wiki.IndexTitle) {
			return nil
		}
		rel, _ := filepath.Rel(repo, p)
		out = append(out, PageRef{Title: title, Path: filepath.ToSlash(rel)})
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Title), strings.ToLower(out[j].Title)
		if a != b {
			return a < b
		}
		return out[i].Path < out[j].Path
	})
	return out, err
}

// findPage returns the page of the project whose title is title, ignoring case (a file system may not tell
// "Ports" from "ports" either).
func findPage(repo, project, title string) (PageRef, bool) {
	ps, _ := pageFiles(repo, project)
	for _, p := range ps {
		if strings.EqualFold(p.Title, title) {
			return p, true
		}
	}
	return PageRef{}, false
}

// listPages is pageFiles with the summary of each page.
func listPages(repo, project string) ([]PageRef, error) {
	out, err := pageFiles(repo, project)
	for i, r := range out {
		if raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(r.Path))); err == nil {
			d := md.Parse(raw)
			out[i].Summary = wiki.ParseMeta(d, r.Title).Summary
			if out[i].Summary == "" {
				out[i].Summary = wiki.FirstSentence(d.Body)
			}
		}
	}
	return out, err
}

// Pending builds one Batch per project that has a waiting note (every project when project is empty): at most max
// of the oldest notes (DefaultMax when max <= 0), every page title with its summary, and up to MaxRelated existing
// pages most related to the notes. It changes nothing.
func Pending(ctx context.Context, st *store.Store, project string, max int) ([]Batch, error) {
	if max <= 0 {
		max = DefaultMax
	}
	projects := noteProjects(st.Repo)
	if project != "" {
		projects = []string{project}
	}
	var out []Batch
	for _, p := range projects {
		ns, err := readNotes(st.Repo, p)
		if err != nil {
			return nil, err
		}
		if len(ns) == 0 {
			continue
		}
		if len(ns) > max {
			ns = ns[:max]
		}
		b := Batch{Project: p, Notes: []Note{}, Related: []Related{}}
		var q []string
		for _, n := range ns {
			b.Notes = append(b.Notes, n.Note)
			q = append(q, truncateRunes(n.Text, queryNote))
		}
		if b.Pages, err = listPages(st.Repo, p); err != nil {
			return nil, err
		}
		if b.Pages == nil {
			b.Pages = []PageRef{}
		}
		b.Related = relatedPages(ctx, st, p, strings.Join(q, "\n"))
		b.Prompt = BuildPrompt(b)
		out = append(out, b)
	}
	return out, nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// relatedPages finds the existing pages of the project that the query is closest to, by the hybrid recall (keywords
// alone when there is no embedder or it fails), in rank order. A page over MaxRelatedBody bytes is skipped.
func relatedPages(ctx context.Context, st *store.Store, project, query string) []Related {
	f := index.Filter{Project: project, Kinds: []string{"wiki"}, Limit: 30}
	var m *index.Matrix
	if st.Emb != nil {
		m, _ = st.DB.LoadMatrix(st.Emb.ID())
	}
	hits, _, _ := st.DB.Hybrid(ctx, st.Emb, m, query, f)
	out := []Related{}
	seen := map[string]bool{}
	for _, h := range hits {
		if len(out) == MaxRelated {
			break
		}
		if seen[h.NodeID] {
			continue
		}
		seen[h.NodeID] = true
		title := strings.TrimSuffix(filepath.Base(h.Path), ".md")
		if strings.EqualFold(title, wiki.IndexTitle) || strings.EqualFold(title, UnsortedTitle) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(st.Repo, filepath.FromSlash(h.Path)))
		if err != nil || len(raw) > MaxRelatedBody {
			continue
		}
		out = append(out, Related{Title: title, Hash: Hash(raw), Body: md.Parse(raw).Body})
	}
	return out
}

func (w Waiting) String() string {
	return fmt.Sprintf("%s: %d notes waiting, oldest %s", w.Project, w.Notes, Age(w.Oldest))
}

// Age writes a duration as a short age: "5 min", "3 h", "2 d".
func Age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}
