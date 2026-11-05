package curate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	Retired    []string     `json:"retired"`     // notes deleted because sessions rated them unhelpful
	Commit     string       `json:"commit,omitempty"`
}

// String is the report as text.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "curate %s: %d notes merged, %d to Unsorted, %d left, %d unknown, %d retired\n", r.Project, r.Merged, r.ToUnsorted, len(r.Left), len(r.Unknown), len(r.Retired))
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
	for _, l := range []struct {
		name string
		ids  []string
	}{{"left", r.Left}, {"unknown", r.Unknown}, {"retired", r.Retired}} {
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

type pageJob struct {
	in      Page
	title   string // the title to write under (the existing file's name when the page exists)
	rel     string
	old     []byte // current file bytes (existing page)
	exists  bool
	notes   []Note
	state   string // "", Merged, Unsorted, Stale, Refused
	problem []string
	out     []byte // the file to write
}

var reNoteID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Submit checks the pages of a model answer and writes them. See the design (section 3.3 and 4): the whole Submit
// runs under the store's write lock and ends in one commit. A page that fails the check is not written; its notes go
// to the Unsorted page as bullets. A stale page and a note that no page took stay pending.
func Submit(ctx context.Context, st *store.Store, sub Submission) (Report, error) {
	project := strings.TrimSpace(sub.Project)
	rep := Report{Project: project, Pages: []PageReport{}, Left: []string{}, Unknown: []string{}, Retired: []string{}}
	if project == "" || strings.ContainsAny(project, "/\\") || strings.HasPrefix(project, ".") {
		return rep, fmt.Errorf("invalid project name %q", project)
	}
	owner := map[string]int{} // a note may be assigned to one page only
	for i, p := range sub.Pages {
		seen := map[string]bool{}
		for _, id := range p.Notes {
			id = strings.TrimSpace(id)
			if seen[id] {
				continue
			}
			seen[id] = true
			if j, dup := owner[id]; dup {
				return rep, fmt.Errorf("note %s is assigned to two pages (%q and %q): nothing written", shortOf(id), sub.Pages[j].Title, p.Title)
			}
			owner[id] = i
		}
	}
	err := st.Do(func(tx *store.Tx) error {
		if tx.Busy() {
			return fmt.Errorf("the memory repository is in the middle of a merge or rebase: nothing written, try again when it is finished")
		}
		repo := tx.Repo()
		all, err := readNotes(repo, project)
		if err != nil {
			return err
		}
		byID := map[string]Note{}
		for _, n := range all {
			byID[n.ID] = n.Note
		}
		date := Now().Format("2006-01-02")

		// Phase A: read each page's current state and decide stale, refused, or candidate.
		jobs := make([]*pageJob, len(sub.Pages))
		titles := map[string]bool{}
		unknown := map[string]bool{}
		assigned := map[string]bool{}
		for i, p := range sub.Pages {
			j := &pageJob{in: p, title: p.Title}
			jobs[i] = j
			seen := map[string]bool{}
			for _, id := range p.Notes {
				id = strings.TrimSpace(id)
				if seen[id] {
					continue
				}
				seen[id] = true
				if n, ok := byID[id]; ok && reNoteID.MatchString(id) {
					j.notes = append(j.notes, n)
					assigned[id] = true
				} else if !unknown[id] {
					unknown[id] = true
					rep.Unknown = append(rep.Unknown, id)
				}
			}
			switch {
			case wiki.ValidTitle(p.Title) != nil:
				j.state, j.problem = Refused, []string{wiki.ValidTitle(p.Title).Error()}
			case strings.EqualFold(p.Title, UnsortedTitle):
				j.state, j.problem = Refused, []string{`the title "Unsorted" belongs to the service`}
			case titles[strings.ToLower(p.Title)]:
				j.state, j.problem = Refused, []string{"two pages of this submission have the same title"}
			}
			if j.state != "" {
				continue
			}
			titles[strings.ToLower(p.Title)] = true
			if ref, ok := findPage(repo, project, p.Title); ok {
				j.exists, j.title, j.rel = true, ref.Title, ref.Path
				if j.old, err = os.ReadFile(filepath.Join(repo, filepath.FromSlash(ref.Path))); err != nil {
					return err
				}
			} else {
				j.rel = "wiki/" + project + "/" + p.Title + ".md"
			}
			switch {
			case p.BaseHash != "" && !j.exists:
				j.state, j.problem = Stale, []string{"the page no longer exists"}
			case p.BaseHash != "" && Hash(j.old) != p.BaseHash:
				j.state, j.problem = Stale, []string{"the page was changed since the batch was made"}
			case p.BaseHash == "" && j.exists:
				j.state, j.problem = Stale, []string{"a page with this title already exists; give its base_hash to change it"}
			}
		}

		// Phase B: the check. A link may name a page created in this submission, so a page whose own check fails
		// takes that name away again from the pages that link to it: repeat until nothing changes.
		linkOK, err := st.DB.LinkResolver(project)
		if err != nil {
			return err
		}
		created := map[string]bool{}
		for _, j := range jobs {
			if j.state == "" && !j.exists {
				created[strings.ToLower(j.title)] = true
			}
		}
		resolve := func(link string) bool {
			l := strings.TrimSpace(link)
			l = strings.TrimSuffix(l, ".md")
			if i := strings.Index(l, "/"); i > 0 && strings.EqualFold(l[:i], project) {
				l = l[i+1:]
			}
			if created[strings.ToLower(l)] || linkOK(link) {
				return true
			}
			_, ok := findPage(repo, project, l) // a page on disk the index has not seen yet
			return ok
		}
		for changed := true; changed; {
			changed = false
			for _, j := range jobs {
				if j.state != "" {
					continue
				}
				var oldBody string
				var fm md.Doc
				if j.exists {
					fm = md.Parse(j.old)
					oldBody = fm.Body
					if fm.Error != "" {
						j.problem = []string{fm.Error}
					}
				}
				if j.problem == nil {
					j.problem = CheckPage(oldBody, j.in.Body, j.notes, resolve)
				}
				if len(j.problem) > 0 {
					j.state = Unsorted
					delete(created, strings.ToLower(j.title))
					changed = true
				}
			}
		}

		// Phase C: compose and write the passing pages, then Unsorted, then delete the notes.
		var bullets []string
		var gone []Note
		for _, j := range jobs {
			pr := PageReport{Title: j.in.Title, Status: j.state, Problems: j.problem}
			for _, n := range j.notes {
				pr.Notes = append(pr.Notes, n.ID)
			}
			switch j.state {
			case "":
				pr.Status = Merged
				data, err := composePage(j, date)
				if err != nil { // the frontmatter of the page cannot be edited: it is treated as a failed check
					pr.Status, pr.Problems = Unsorted, []string{err.Error()}
				} else if err := tx.Write(j.rel, data); err != nil {
					return err
				}
			case Stale:
				rep.Left = append(rep.Left, pr.Notes...)
				rep.Pages = append(rep.Pages, pr)
				continue
			}
			switch pr.Status {
			case Merged:
				rep.Merged += len(j.notes)
				gone = append(gone, j.notes...)
			default: // Unsorted, Refused
				rep.ToUnsorted += len(j.notes)
				for _, n := range j.notes {
					bullets = append(bullets, fmt.Sprintf("- %s (%s, %s)", flat(n.Text), date, short(n.ID)))
				}
				gone = append(gone, j.notes...)
			}
			rep.Pages = append(rep.Pages, pr)
		}
		if len(bullets) > 0 {
			rel, data, err := appendUnsorted(repo, project, bullets)
			if err != nil {
				return err
			}
			if err := tx.Write(rel, data); err != nil {
				return err
			}
		}
		for _, n := range gone {
			if err := tx.Delete("notes/"+project+"/"+n.ID, project, "", ""); err != nil {
				return err
			}
		}
		for _, n := range all {
			if !assigned[n.ID] {
				rep.Left = append(rep.Left, n.ID)
			}
		}

		// Index page, rating retirement, event, one commit
		if len(tx.Changed()) > 0 { // a submit that wrote nothing leaves the Index alone
			if err := tx.IndexPage(project); err != nil {
				return err
			}
		}
		for _, id := range st.RetireCandidates() {
			if !strings.HasPrefix(id, "notes/"+project+"/") {
				continue
			}
			if _, err := os.Stat(filepath.Join(repo, filepath.FromSlash(id)+".md")); err != nil {
				continue
			}
			if err := tx.Delete(id, project, "retire", "rated unhelpful"); err != nil {
				return err
			}
			rep.Retired = append(rep.Retired, filepath.Base(id))
		}
		if len(rep.Retired) > 0 { // a note that was just deleted is not pending any more
			gone := map[string]bool{}
			for _, id := range rep.Retired {
				gone[id] = true
			}
			left := rep.Left[:0]
			for _, id := range rep.Left {
				if !gone[id] {
					left = append(left, id)
				}
			}
			rep.Left = left
		}
		mergedPages := 0
		for _, p := range rep.Pages {
			if p.Status == Merged {
				mergedPages++
			}
		}
		rep.Commit = fmt.Sprintf("curate %s: %d notes into %d pages (%d to Unsorted)", project, rep.Merged, mergedPages, rep.ToUnsorted)
		st.Event("curate", "", project, fmt.Sprintf("merged=%d pages=%d unsorted=%d left=%d unknown=%d retired=%d", rep.Merged, mergedPages, rep.ToUnsorted, len(rep.Left), len(rep.Unknown), len(rep.Retired)))
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
	})
	return rep, err
}

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
		fm := map[string]any{"title": j.title}
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
	text := strings.ReplaceAll(string(j.old), "\r\n", "\n")
	d := md.Parse([]byte(text))
	head := text[:len(text)-len(d.Body)]
	set := map[string]any{}
	if summary != "" {
		set["summary"] = summary
	}
	meta := wiki.ParseMeta(d, j.title)
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
func appendUnsorted(repo, project string, bullets []string) (string, []byte, error) {
	add := strings.Join(bullets, "\n") + "\n"
	if ref, ok := findPage(repo, project, UnsortedTitle); ok {
		raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(ref.Path)))
		if err != nil {
			return "", nil, err
		}
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			raw = append(raw, '\n')
		}
		return ref.Path, append(raw, add...), nil
	}
	fm := map[string]any{"title": UnsortedTitle, "summary": UnsortedSummary}
	return "wiki/" + project + "/" + UnsortedTitle + ".md", md.Render(fm, add), nil
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
