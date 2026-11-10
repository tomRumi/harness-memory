package curate

import (
	"strings"

	"github.com/tomRumi/harness-memory/internal/md"
	"github.com/tomRumi/harness-memory/internal/store"
	"github.com/tomRumi/harness-memory/internal/wiki"
)

// pageJob is one submitted page. Its report entry is its only state: Status is empty while the page passes every
// check and becomes Merged, Unsorted, Stale or Refused when the plan is done.
type pageJob struct {
	PageReport
	in        Page
	fileTitle string // the title to write under (the existing file's name when the page exists)
	rel       string
	old       []byte // current file bytes (existing page)
	exists    bool
	notes     []Note
}

// mark gives the page its final status and the reason.
func (j *pageJob) mark(status, problem string) { j.Status, j.Problems = status, []string{problem} }

// pageFile is a page on disk as it was read under the lock.
type pageFile struct {
	PageRef
	raw []byte
	err error
}

// world is everything the plan looks at: the waiting notes, the pages on disk (one set, read under the write lock),
// and three questions about the repository. plan touches nothing.
type world struct {
	project, date string
	notes         map[string]Note
	pages         []pageFile
	unverified    func(id string) bool   // the note is on disk and not checked yet
	linkOK        func(link string) bool // the index's link rule
	taken         func(rel string) bool  // anything at rel on disk
	ignored       func(rel string) bool  // git would ignore rel
}

// page finds the page whose title is title, up to case and Unicode normalisation.
func (w *world) page(title string) (pageFile, bool) {
	for _, p := range w.pages {
		if wiki.Fold(p.Title) == wiki.Fold(title) {
			return p, true
		}
	}
	return pageFile{}, false
}

// plan decides, for each submitted page, whether it is merged, goes to Unsorted, is stale or is refused, and why.
// It also returns the ids that are not notes of the project. ids are the cleaned note ids of each page.
func (w *world) plan(sub Submission, ids [][]string) (jobs []*pageJob, unknown []string, err error) {
	titles, seen := map[string]bool{}, map[string]bool{}
	for i, p := range sub.Pages {
		j := &pageJob{PageReport: PageReport{Title: p.Title}, in: p, fileTitle: p.Title}
		jobs = append(jobs, j)
		var waiting []string // named notes that are unverified
		for _, id := range ids[i] {
			if n, ok := w.notes[id]; ok && usableID(id) {
				j.notes = append(j.notes, n)
				j.Notes = append(j.Notes, id)
			} else if usableID(id) && w.unverified(id) {
				waiting = append(waiting, short(id))
			} else if !seen[id] {
				seen[id] = true
				unknown = append(unknown, id)
			}
		}
		if err := w.place(j, titles); err != nil {
			return nil, nil, err
		}
		if len(waiting) > 0 && j.Status == "" { // the page would carry unchecked text: it and its notes stay as they are
			j.mark(Stale, "names unverified notes ("+strings.Join(waiting, " ")+"); verify them first")
		}
	}
	w.checkAll(jobs)
	return jobs, unknown, nil
}

// place decides what can be said about a page before its text is checked: refused, stale, or a candidate.
func (w *world) place(j *pageJob, titles map[string]bool) error {
	p, fold := j.in, wiki.Fold(j.in.Title)
	if err := wiki.ValidModelTitle(p.Title); err != nil {
		j.mark(Refused, err.Error())
	} else if titles[fold] {
		j.mark(Refused, "two pages of this submission have the same title")
	}
	if j.Status != "" {
		return nil
	}
	titles[fold] = true
	if ref, ok := w.page(p.Title); ok {
		j.exists, j.fileTitle, j.rel, j.old = true, ref.Title, ref.Path, ref.raw
		if ref.err != nil {
			return ref.err
		}
	} else {
		j.rel = "wiki/" + w.project + "/" + p.Title + ".md"
	}
	switch {
	case !j.exists && w.taken(j.rel):
		// the file system is the final truth for "exists": two names that fold differently in our table can
		// still be one file for it, and a new page is never written over a file
		j.mark(Stale, "a file with this name already exists; give the base_hash of that page to change it")
	case !j.exists && w.ignored(j.rel):
		j.mark(Unsorted, store.ErrIgnored.Error())
	case p.BaseHash != "" && !j.exists:
		j.mark(Stale, "the page no longer exists")
	case p.BaseHash != "" && Hash(j.old) != p.BaseHash:
		j.mark(Stale, "the page was changed since the batch was made")
	case p.BaseHash == "" && j.exists:
		j.mark(Stale, "a page with this title already exists; give its base_hash to change it")
	}
	return nil
}

// checkAll runs the check on every candidate. A link may name a page created in this submission, so a page whose own
// check fails takes that name away again from the pages that link to it: repeat until nothing changes. What passes
// ends as Merged.
func (w *world) checkAll(jobs []*pageJob) {
	created := map[string]bool{}
	for _, j := range jobs {
		if j.Status == "" && !j.exists {
			created[wiki.Fold(j.fileTitle)] = true
		}
	}
	resolve := func(link string) bool {
		l := strings.TrimSuffix(strings.TrimSpace(link), ".md")
		if i := strings.Index(l, "/"); i > 0 && wiki.Fold(l[:i]) == wiki.Fold(w.project) {
			l = l[i+1:]
		}
		return created[wiki.Fold(l)] || w.linkOK(link)
	}
	for changed := true; changed; {
		changed = false
		for _, j := range jobs {
			if j.Status != "" {
				continue
			}
			if probs := j.check(resolve); len(probs) > 0 {
				j.Status, j.Problems = Unsorted, probs
				delete(created, wiki.Fold(j.fileTitle))
				changed = true
			}
		}
	}
	for _, j := range jobs {
		if j.Status == "" {
			j.Status = Merged
		}
	}
}

// check is the problems of the page text, none when it passes.
func (j *pageJob) check(resolve func(string) bool) []string {
	oldBody := ""
	if j.exists {
		d := md.Parse(j.old)
		if d.Error != "" {
			return []string{d.Error}
		}
		oldBody = d.Body
	}
	return CheckPage(oldBody, j.in.Body, j.notes, resolve)
}
