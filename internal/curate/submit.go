package curate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tomRumi/harness-memory-service/internal/index"
	"github.com/tomRumi/harness-memory-service/internal/md"
	"github.com/tomRumi/harness-memory-service/internal/store"
	"github.com/tomRumi/harness-memory-service/internal/wiki"
)

// Now is the clock of the merge (dates in sources and in Unsorted); a test may replace it.
var Now = time.Now

// UnsortedSummary is the summary of the Unsorted page the service creates.
const UnsortedSummary = "Notes the merge could not place; sort them into pages"

// Page results.
const (
	Merged   = "merged"   // the page was written and its notes deleted
	Unsorted = "unsorted" // the page failed; its notes went to the page Unsorted as bullets
	Stale    = "stale"    // the page changed since the batch was made, or already exists; its notes stay pending
	Refused  = "refused"  // the title is not allowed; its notes went to Unsorted
)

// PageReport is the result for one submitted page.
type PageReport struct {
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Problems []string `json:"problems,omitempty"`
	Notes    []string `json:"notes,omitempty"` // ids of the notes assigned to the page that exist
}

// Report is the result of a Submit.
type Report struct {
	Project    string       `json:"project"`
	Pages      []PageReport `json:"pages"`
	Merged     int          `json:"merged"`      // notes merged into pages
	ToUnsorted int          `json:"to_unsorted"` // notes put into Unsorted
	Left       []string     `json:"left"`        // notes still pending: not assigned, or on a stale page
	Unknown    []string     `json:"unknown"`     // ids that are not notes of the project
	Warnings   []string     `json:"warnings"`    // things worth a look that did not stop the write (a new page that copies another)
	Commit     string       `json:"commit,omitempty"`
}

// String is the report as text.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "curate %s: %d notes merged, %d to Unsorted, %d left, %d unknown\n", r.Project, r.Merged, r.ToUnsorted, len(r.Left), len(r.Unknown))
	for _, p := range r.Pages {
		st := p.Status
		if st == Refused {
			st = "refused(" + p.Title + ")"
			fmt.Fprintf(&b, "  %s, %d notes\n", st, len(p.Notes))
		} else {
			fmt.Fprintf(&b, "  %-8s %s, %d notes\n", st, p.Title, len(p.Notes))
		}
		for _, pr := range p.Problems {
			fmt.Fprintf(&b, "    - %s\n", pr)
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	for _, l := range []struct {
		name string
		ids  []string
	}{{"left", r.Left}, {"unknown", r.Unknown}} {
		if len(l.ids) > 0 {
			names := make([]string, len(l.ids))
			for i, id := range l.ids {
				names[i] = id
				if l.name != "unknown" {
					names[i] = shortOf(id)
				}
			}
			fmt.Fprintf(&b, "  %s: %s\n", l.name, strings.Join(names, " "))
		}
	}
	return b.String()
}

func shortOf(id string) string {
	id = filepath.Base(id)
	return short(strings.TrimSuffix(id, ".md"))
}

// Submit checks the pages of a model answer and writes them. See the design (section 3.3 and 4): the whole Submit
// runs under the store's write lock and ends in one commit. A page that fails the check is not written; its notes go
// to the Unsorted page as bullets. A stale page and a note that no page took stay pending.
func Submit(ctx context.Context, st *store.Store, sub Submission) (Report, error) {
	project := strings.TrimSpace(sub.Project)
	rep := Report{Project: project, Pages: []PageReport{}, Left: []string{}, Unknown: []string{}, Warnings: []string{}}
	if project == "" {
		return rep, fmt.Errorf("invalid project name %q", project)
	}
	project, err := store.ValidProject(project) // the same rule as every other write; no wiki/PROJ is ever made
	if err != nil {
		return rep, err
	}
	rep.Project = project
	ids, err := assignedIDs(sub)
	if err != nil {
		return rep, err
	}
	var newPages []string // titles of the pages created, for the copy warning after the lock is released
	err = st.Do(func(tx *store.Tx) error {
		if tx.Busy() {
			return fmt.Errorf("the memory repository is in the middle of a merge or rebase: nothing written, try again when it is finished")
		}
		w, all, err := loadWorld(st, tx, project)
		if err != nil {
			return err
		}
		jobs, unknown, err := w.plan(sub, ids)
		if err != nil {
			return err
		}
		rep.Unknown = append(rep.Unknown, unknown...)
		var gone []Note
		if newPages, gone, err = apply(tx, w, jobs, all, &rep); err != nil {
			return err
		}
		return finish(ctx, tx, &rep, gone)
	})
	if err != nil {
		return rep, err
	}
	// Commit now, before the slow copy check: a kill from here on leaves the files committed, not loose.
	if st.Commit != nil {
		st.Commit.Flush()
		if err := st.Commit.LastErr(); err != nil {
			rep.Warnings = append(rep.Warnings, CommitFailed(err))
		}
	}
	var mat *index.Matrix // vectors of the existing pages, loaded for the first new page
	for _, title := range newPages {
		if w := copyWarning(ctx, st, project, title, &mat); w != "" {
			rep.Warnings = append(rep.Warnings, w)
		}
	}
	return rep, nil
}

// assignedIDs returns the note ids of each submitted page, trimmed and without repeats. A note may be assigned to
// one page only.
func assignedIDs(sub Submission) ([][]string, error) {
	owner := map[string]int{}
	out := make([][]string, len(sub.Pages))
	for i, p := range sub.Pages {
		for _, id := range p.Notes {
			id = strings.TrimSpace(id)
			if j, ok := owner[id]; ok && j == i {
				continue
			} else if ok {
				return nil, fmt.Errorf("note %s is assigned to two pages (%q and %q): nothing written", shortOf(id), sub.Pages[j].Title, p.Title)
			}
			owner[id] = i
			out[i] = append(out[i], id)
		}
	}
	return out, nil
}

// loadWorld reads, under the lock, everything the plan needs: the waiting notes, the pages on disk (after the index
// has been brought in line with them) and the git and file-system questions.
func loadWorld(st *store.Store, tx *store.Tx, project string) (*world, []waitingNote, error) {
	repo := tx.Repo()
	all, err := readNotes(repo, project)
	if err != nil {
		return nil, nil, err
	}
	if _, err := st.DB.Rescan(repo); err != nil { // the index is the link rule; it must see the files that are there
		return nil, nil, err
	}
	linkOK, err := st.DB.LinkResolver(project)
	if err != nil {
		return nil, nil, err
	}
	w := &world{project: project, date: Now().Format("2006-01-02"), notes: map[string]Note{}, linkOK: linkOK, ignored: tx.Ignored,
		taken: func(rel string) bool { return pathTaken(repo, rel) }}
	for _, n := range all {
		w.notes[n.ID] = n.Note
	}
	refs, _ := pageFiles(repo, project)
	for _, r := range refs {
		raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(r.Path)))
		w.pages = append(w.pages, pageFile{r, raw, err})
	}
	return w, all, nil
}

// apply writes the decisions of the plan: the passing pages, the Unsorted bullets, the deletion of the merged notes.
// It fills the report and returns the titles of the pages it created.
func apply(tx *store.Tx, w *world, jobs []*pageJob, all []waitingNote, rep *Report) (newPages []string, gone []Note, err error) {
	var bullets []string
	assigned := map[string]bool{}
	for _, j := range jobs {
		if j.Status == Merged {
			created, err := w.write(tx, j)
			if err != nil {
				return nil, nil, err
			}
			if created {
				newPages = append(newPages, j.fileTitle)
			}
		}
		for _, n := range j.notes {
			assigned[n.ID] = true
		}
		switch j.Status {
		case Stale:
			rep.Left = append(rep.Left, j.Notes...)
		case Merged:
			rep.Merged += len(j.notes)
			gone = append(gone, j.notes...)
		default: // Unsorted, Refused
			rep.ToUnsorted += len(j.notes)
			for _, n := range j.notes {
				bullets = append(bullets, fmt.Sprintf("- %s (%s, %s)", flat(n.Text), w.date, short(n.ID)))
			}
			gone = append(gone, j.notes...)
		}
		rep.Pages = append(rep.Pages, j.PageReport)
	}
	if len(bullets) > 0 {
		rel, data := w.appendUnsorted(bullets)
		if err := tx.Write(rel, data); err != nil {
			return nil, nil, err
		}
	}
	for _, n := range gone {
		if err := tx.Delete("notes/"+w.project+"/"+n.ID, w.project, "", ""); err != nil {
			return nil, nil, err
		}
	}
	for _, n := range all {
		if !assigned[n.ID] {
			rep.Left = append(rep.Left, n.ID)
		}
	}
	return newPages, gone, nil
}

// write composes and writes a passing page. A page that cannot be written after all (an earlier page of this
// submission holds the file, or its frontmatter cannot be edited) becomes Unsorted. created says a new page was made.
func (w *world) write(tx *store.Tx, j *pageJob) (created bool, err error) {
	data, cerr := composePage(j, w.date)
	switch {
	case !j.exists && w.taken(j.rel):
		j.mark(Unsorted, "another page of this submission is written to the same file")
	case cerr != nil: // the frontmatter of the page cannot be edited: it is treated as a failed check
		j.mark(Unsorted, cerr.Error())
	default:
		return !j.exists, tx.Write(j.rel, data)
	}
	return false, nil
}

// finish regenerates the Index page, records the event and tells the committer, once, with the commit message.
func finish(ctx context.Context, tx *store.Tx, rep *Report, gone []Note) error {
	project := rep.Project
	if len(tx.Changed()) > 0 { // a submit that wrote nothing leaves the Index alone
		if err := tx.IndexPage(project); errors.Is(err, store.ErrHandWritten) {
			rep.Warnings = append(rep.Warnings, err.Error())
		} else if err != nil {
			return err
		}
	}
	mergedPages := 0
	for _, p := range rep.Pages {
		if p.Status == Merged {
			mergedPages++
		}
	}
	rep.Commit = fmt.Sprintf("curate %s: %d notes into %d pages (%d to Unsorted)", project, rep.Merged, mergedPages, rep.ToUnsorted)
	tx.Event("curate", "", project, fmt.Sprintf("merged=%d pages=%d unsorted=%d left=%d unknown=%d", rep.Merged, mergedPages, rep.ToUnsorted, len(rep.Left), len(rep.Unknown)))
	msg := rep.Commit
	if len(gone) > 0 {
		ids := make([]string, len(gone))
		for i, n := range gone {
			ids[i] = n.ID
		}
		msg += "\n\nnotes: " + strings.Join(ids, " ")
	}
	tx.Finish(ctx, msg)
	return nil
}

// pathTaken reports whether anything (a file, a link, a folder) is at rel. Under the write lock this is exactly "a page
// is there", whatever its title looks like.
func pathTaken(repo, rel string) bool {
	_, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(rel)))
	return err == nil
}

// CommitFailed is the sentence every command prints when the files are saved but the commit failed.
func CommitFailed(err error) string {
	return fmt.Sprintf("committed: NO (%s); the files are saved, run hms doctor", firstLine(err.Error()))
}

func firstLine(s string) string { return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0]) }

// composePage builds the file of a passing page. The frontmatter comes from the service: a new page gets title,
// summary, tags and sources; an existing page keeps its frontmatter byte for byte except summary (when given), tags
// (the union) and sources (one entry added).
func composePage(j *pageJob, date string) ([]byte, error) {
	body := strings.TrimRight(j.in.Body, "\n \t") + "\n"
	summary := strings.Join(strings.Fields(j.in.Summary), " ")
	tags := union(nil, j.in.Tags)
	var entry string
	if len(j.notes) > 0 {
		ids := make([]string, len(j.notes))
		for i, n := range j.notes {
			ids[i] = short(n.ID)
		}
		entry = date + ": " + strings.Join(ids, " ")
	}
	if !j.exists {
		fm := map[string]any{"title": j.fileTitle}
		if summary != "" {
			fm["summary"] = summary
		}
		if len(tags) > 0 {
			fm["tags"] = tags
		}
		if entry != "" {
			fm["sources"] = []string{entry}
		}
		return md.Render(fm, body), nil
	}
	d := md.Parse(j.old)
	head := d.Head
	set := map[string]any{}
	if summary != "" {
		set["summary"] = summary
	}
	meta := wiki.ParseMeta(d, j.fileTitle)
	if merged := union(meta.Tags, tags); len(merged) > len(meta.Tags) {
		set["tags"] = merged
	}
	if entry != "" {
		set["sources"] = append(meta.Sources, entry)
	}
	if len(set) == 0 {
		return []byte(head + body), nil
	}
	out, err := md.SetKeys([]byte(head), set)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return append(out, []byte("\n"+body)...), nil
	}
	return append(out, []byte(body)...), nil
}

// appendUnsorted returns the path and new content of the Unsorted page with the bullets added at the end. The page is
// created when it is missing; existing lines are not touched.
func (w *world) appendUnsorted(bullets []string) (string, []byte) {
	add := strings.Join(bullets, "\n") + "\n"
	if pf, ok := w.page(UnsortedTitle); ok {
		raw := pf.raw
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			raw = append(raw, '\n')
		}
		return pf.Path, append(raw, add...)
	}
	fm := map[string]any{"title": UnsortedTitle, "summary": UnsortedSummary}
	return "wiki/" + w.project + "/" + UnsortedTitle + ".md", md.Render(fm, add)
}

func union(a, b []string) []string {
	out := append([]string(nil), a...)
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if x = strings.TrimSpace(x); x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// copyWarning (run after the write lock is released, with the embedding timeout of the store) says so when the new page title (just written and indexed, not yet embedded) has a page vector within
// index.DuplicateThreshold of another page of the project. It is only advice: any failure to compute it gives "".
func copyWarning(ctx context.Context, st *store.Store, project, title string, mat **index.Matrix) string {
	if st.Emb == nil {
		return ""
	}
	id := "wiki/" + project + "/" + title
	ctx, cancel := context.WithTimeout(ctx, store.EmbedTimeout)
	defer cancel()
	v, err := st.DB.NodeVector(ctx, st.Emb, id)
	if err != nil {
		return ""
	}
	if *mat == nil {
		m, err := st.DB.LoadMatrix(st.Emb.ID())
		if err != nil {
			return ""
		}
		*mat = m
	}
	best, ok := (*mat).MostSimilar(v, project, id)
	if !ok || best.Score < index.DuplicateThreshold {
		return ""
	}
	return fmt.Sprintf("page %q looks like a copy of [[%s]] (%.2f); consider merging them", title, best.Title, best.Score)
}
