package curate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tomRumi/harness-memory-service/internal/store"
)

// Page is one page of a model answer: the whole new body of a page, and the notes it takes in. The same shape
// travels from a harness job and from the built-in model.
type Page struct {
	Title    string   `json:"title"`
	Summary  string   `json:"summary,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Body     string   `json:"body"`
	Notes    []string `json:"notes"`
	BaseHash string   `json:"base_hash,omitempty"` // the hash from "related" for an existing page; empty for a new page
}

// Submission is what hms curate -submit takes: the pages of one project.
type Submission struct {
	Project string `json:"project"`
	Pages   []Page `json:"pages"`
}

// ParseAnswer reads a model answer, {"pages":[...]}. It tolerates a ```json fence and text around the JSON: the first
// JSON object that has a "pages" key is taken.
func ParseAnswer(s string) ([]Page, error) {
	sub, err := parseObject(s)
	if err != nil {
		return nil, err
	}
	return sub.Pages, nil
}

// ParseSubmission reads a Submission, or a bare model answer when project is given (-p). A project inside the text
// that differs from the given one is an error.
func ParseSubmission(s, project string) (Submission, error) {
	sub, err := parseObject(s)
	if err != nil {
		return sub, err
	}
	switch {
	case sub.Project == "":
		sub.Project = project
	case project != "" && sub.Project != project:
		return sub, fmt.Errorf("the submission is for project %q but -p says %q", sub.Project, project)
	}
	if sub.Project == "" {
		return sub, fmt.Errorf(`the submission has no "project" (or give -p)`)
	}
	return sub, nil
}

func parseObject(s string) (Submission, error) {
	var last error
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		var raw struct {
			Project string  `json:"project"`
			Pages   *[]Page `json:"pages"`
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		if err := dec.Decode(&raw); err != nil {
			last = err
			continue
		}
		if raw.Pages == nil {
			last = fmt.Errorf(`the JSON object has no "pages"`)
			continue
		}
		return Submission{Project: raw.Project, Pages: *raw.Pages}, nil
	}
	if last != nil {
		return Submission{}, fmt.Errorf("no usable JSON answer found: %v", last)
	}
	return Submission{}, fmt.Errorf("no JSON object found in the answer")
}

// BuildPrompt returns the instruction for the model that places a batch of notes into wiki pages. The data (notes,
// pages, related) is not repeated in it: a harness reads them from the batch, and ModelMessage appends them for the
// built-in model.
func BuildPrompt(b Batch) string {
	return fmt.Sprintf(`You maintain the wiki of the project %q. The input has "notes" (new, unsorted observations, each with an id), "pages" (the titles and one-line summaries of every existing page) and "related" (the full body and hash of existing pages that look related to the notes).

Place every note into the best page: an existing page given in "related", or a new page.
- Return the WHOLE new body of every page you change: for an existing page, its text exactly as given plus your insertions; do not rewrite, reorder, shorten or delete anything that is already there. Do not return pages you do not change.
- Keep every number, name, path and command of a note exactly as written. Put the note's sentence (a lightly edited sentence is fine, an 8-word run of it must stay verbatim) in the section of the page where it belongs.
- Use [[Page Title]] links only to titles listed in "pages" or to pages you create in this same answer.
- Give each new page a short plain title (no slash, no leading dot, at most 100 characters; not "Index" or "Unsorted") and a one-line summary.
- Assign every note to exactly one page by listing its id in that page's "notes".
- For an existing page, copy "base_hash" from "related"; for a new page leave it empty. Do not write frontmatter.
- Answer with JSON only, no other text.

Schema:
{"pages":[{"title":"Page Title","summary":"one line","tags":["tag"],"body":"whole markdown body","notes":["note id"],"base_hash":"hash from related, or empty for a new page"}]}

Example, one note placed into an existing page and one into a new page:
{"pages":[{"title":"Ports","summary":"Ports the services use","tags":["network"],"body":"# Ports\n\nThe daemon listens on 8765.\n\n- The worker pool has 4 threads and restarts after a crash.\n","notes":["3f2a9c1e-0000-0000-0000-000000000001"],"base_hash":"<hash from related>"},{"title":"Backups","summary":"How and when backups run","tags":[],"body":"# Backups\n\nThe nightly backup runs at 02:30 and keeps 7 copies.\n","notes":["3f2a9c1e-0000-0000-0000-000000000002"],"base_hash":""}]}
`, b.Project)
}

// ModelMessage is the whole message for a model that has no other access to the batch: the instruction, then the
// notes, pages and related pages as JSON.
func ModelMessage(b Batch) string {
	in := b
	in.Prompt = ""
	data, _ := json.MarshalIndent(in, "", " ")
	return b.Prompt + "\nInput:\n" + string(data) + "\n"
}

// Applied is the result of -apply for one project: a report, or the reason the model's answer was not submitted.
type Applied struct {
	Project string
	Report  *Report
	Err     error // the model failed or its answer was not understood: nothing was submitted, the notes stay pending
}

// Apply merges with the built-in model: for each project that has waiting notes, Pending, the model, ParseAnswer,
// Submit. A model error or an answer that cannot be parsed leaves that project's notes pending (there is no fallback
// to Unsorted: nothing was submitted). A Submit error is reported the same way.
func Apply(ctx context.Context, st *store.Store, llm *LLM, project string, max int) ([]Applied, error) {
	batches, err := Pending(ctx, st, project, max)
	if err != nil {
		return nil, err
	}
	var out []Applied
	for _, b := range batches {
		a := Applied{Project: b.Project}
		ans, err := llm.Complete(ctx, ModelMessage(b))
		var pages []Page
		if err == nil {
			if pages, err = ParseAnswer(ans); err != nil {
				err = fmt.Errorf("the model's answer was not understood: %v", err)
			}
		}
		if err == nil {
			var r Report
			if r, err = Submit(ctx, st, Submission{Project: b.Project, Pages: pages}); err == nil {
				a.Report = &r
			}
		}
		a.Err = err
		out = append(out, a)
	}
	return out, nil
}
