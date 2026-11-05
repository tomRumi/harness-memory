// Package curate turns piles of small notes into wiki entries. It follows the predecessor (consolidate.py): notes are
// grouped by project and kind prefix ("rag:gotcha" belongs to "rag"), a model weaves them into the wiki, a check that does
// not use a model looks at the result, and on any failure the notes are appended as plain bullets instead.
//
// Differences from the predecessor, by design: the model writes ONE new section that is appended (the existing text is
// never rewritten); merged notes are marked superseded, never deleted; the run is a dry run unless asked to apply; and
// only the machine marked as curation owner merges anything.
package curate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/tomRumi/harness-memory-service/internal/gate"
	"github.com/tomRumi/harness-memory-service/internal/store"
)

// Writer-free operation. The service does not need a model of its own: a harness (a scheduled Hermes job, Claude Code, any
// agent) can ask for the ready batches with Pending, write each section with its own model, and hand it back with Submit.
// Submit runs the same check as a model call inside the service and falls back to plain bullets in the same way.

// Merger writes one section from notes. existing is the text of the entry (empty when it does not exist yet).
type Merger interface {
	Section(ctx context.Context, existing string, notes []gate.Note) (string, error)
}

// Config is the machine-local curation setting (<home>/config.yaml). It is not in the memory repository: the owner
// differs from machine to machine.
type Config struct {
	Curation struct {
		Owner bool `yaml:"owner"`
		Batch int  `yaml:"batch"`
	} `yaml:"curation"`
}

// BatchSize is the number of active notes of one project and prefix that triggers a merge, and the number merged.
const BatchSize = 10

// LoadConfig reads <home>/config.yaml; a missing file means this machine is not the curation owner.
func LoadConfig(home string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config.yaml: %w", err)
	}
	return c, nil
}

var prefixRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Batch is the notes of one project and prefix that are ready to merge.
type Batch struct {
	Project, Prefix string
	Target          string // wiki/<project>/modules/<prefix>.md
	IDs             []string
	Missing         bool // the entry does not exist yet
}

// Outcome of one batch.
type Outcome struct {
	Batch
	Result   string // "merged", "fallback", "scaffold", "planned"
	Problems []string
}

// Report is what a run did, or in a dry run would do.
type Report struct {
	Owner    bool
	Applied  bool
	Outcomes []Outcome
	Manual   int // active notes without a usable prefix: never merged automatically
	Waiting  int // active notes in groups that have not reached the batch size
	Retired  []string
	Skipped  string // why nothing was done, when that is so
}

// Merged is the number of notes merged in this run.
func (r Report) Merged() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Result == "merged" || o.Result == "fallback" || o.Result == "scaffold" {
			n += len(o.IDs)
		}
	}
	return n
}

// Plan finds the batches that are ready. project limits the search; empty means every project.
func Plan(st *store.Store, project string) (batches []Batch, manual, waiting int, err error) {
	q := `SELECT id, project, note_kind, created_at FROM nodes WHERE kind='note' AND status='active'`
	var args []any
	if project != "" {
		q += ` AND project=?`
		args = append(args, project)
	}
	rows, err := st.DB.Query(q, args...)
	if err != nil {
		return nil, 0, 0, err
	}
	type note struct{ id, created string }
	groups := map[[2]string][]note{}
	for rows.Next() {
		var id, proj, kind, created string
		if err := rows.Scan(&id, &proj, &kind, &created); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		i := strings.Index(kind, ":")
		if i < 0 || !prefixRe.MatchString(kind[:i]) {
			manual++ // no prefix, or one that is not a plain name: never merged automatically
			continue
		}
		prefix := kind[:i]
		k := [2]string{proj, prefix}
		groups[k] = append(groups[k], note{id, created})
	}
	rows.Close()
	keys := make([][2]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		ns := groups[k]
		if len(ns) < BatchSize {
			waiting += len(ns)
			continue
		}
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].created != ns[j].created {
				return ns[i].created < ns[j].created
			}
			return ns[i].id < ns[j].id
		})
		b := Batch{Project: k[0], Prefix: k[1], Target: "wiki/" + k[0] + "/modules/" + k[1] + ".md"}
		for _, n := range ns[:BatchSize] {
			b.IDs = append(b.IDs, n.id)
		}
		waiting += len(ns) - BatchSize
		if _, err := os.Stat(filepath.Join(st.Repo, filepath.FromSlash(b.Target))); err != nil {
			b.Missing = true
		}
		batches = append(batches, b)
	}
	return batches, manual, waiting, nil
}

// bareID is the last path element of a node id: the note's own id.
func bareID(node string) string { return node[strings.LastIndex(node, "/")+1:] }

// Fallback is the section used when the model fails or the check refuses its text: one bullet per note, whole, with its
// citation. It cannot fail the check.
func Fallback(notes []gate.Note) string {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString("- " + strings.Join(strings.Fields(n.Text), " ") + " [[" + n.ID + "]]\n")
	}
	return b.String()
}

// Run merges every batch that is ready. Without apply it only reports. Only the owner merges; a repository in the middle
// of a merge or rebase is left alone.
func Run(ctx context.Context, st *store.Store, m Merger, cfg Config, apply bool, project string) (Report, error) {
	rep := Report{Owner: cfg.Curation.Owner, Applied: apply}
	batches, manual, waiting, err := Plan(st, project)
	if err != nil {
		return rep, err
	}
	rep.Manual, rep.Waiting = manual, waiting
	if apply && !cfg.Curation.Owner {
		rep.Skipped = "this machine is not the curation owner (curation.owner in config.yaml); nothing merged"
		for _, b := range batches {
			rep.Outcomes = append(rep.Outcomes, Outcome{Batch: b, Result: "planned"})
		}
		return rep, nil
	}
	if apply && st.Commit != nil && st.Commit.Busy() {
		rep.Skipped = "the repository is in the middle of a merge or rebase; nothing merged"
		return rep, nil
	}
	for _, b := range batches {
		o := Outcome{Batch: b, Result: "planned"}
		if apply {
			if o, err = runBatch(ctx, st, m, b); err != nil {
				return rep, err
			}
		}
		rep.Outcomes = append(rep.Outcomes, o)
	}
	if apply {
		for _, id := range st.RetireCandidates() {
			msg, err := st.Retire(ctx, bareID(id), fmt.Sprintf("rated unhelpful by %d sessions, helpful by none", store.RetireAfter), "")
			if err != nil {
				return rep, err
			}
			rep.Retired = append(rep.Retired, msg)
		}
	}
	return rep, nil
}

func batchNotes(st *store.Store, b Batch) ([]gate.Note, error) {
	var notes []gate.Note
	for _, id := range b.IDs {
		t, err := st.NoteText(id)
		if err != nil {
			return nil, err
		}
		notes = append(notes, gate.Note{ID: bareID(id), Text: t})
	}
	return notes, nil
}

// PendingBatch is a ready batch with everything a writer needs. A harness returns Target and IDs unchanged, plus Section.
type PendingBatch struct {
	Project string      `json:"project"`
	Target  string      `json:"target"`
	IDs     []string    `json:"ids"`
	New     bool        `json:"new_entry"` // true: the service writes it itself, no section needed
	Entry   string      `json:"entry"`     // the current text of the wiki entry
	Notes   []gate.Note `json:"notes"`
	Prompt  string      `json:"prompt"`
}

// Pending lists the ready batches that need a writer.
func Pending(st *store.Store, project string) ([]PendingBatch, error) {
	batches, _, _, err := Plan(st, project)
	if err != nil {
		return nil, err
	}
	out := []PendingBatch{}
	for _, b := range batches {
		notes, err := batchNotes(st, b)
		if err != nil {
			return nil, err
		}
		pb := PendingBatch{Project: b.Project, Target: b.Target, IDs: b.IDs, New: b.Missing, Notes: notes}
		if !b.Missing {
			if pb.Entry, err = st.WikiRead(b.Target); err != nil {
				return nil, err
			}
			pb.Prompt = BuildPrompt(pb.Entry, notes)
		}
		out = append(out, pb)
	}
	return out, nil
}

// Submit takes a section written elsewhere for one ready batch, checks it and appends it. The batch must still be ready:
// the notes must be active and be the notes the service would pick now. A section that fails the check, or an empty one,
// is replaced by plain bullets, so a poor writer cannot lose a note.
func Submit(ctx context.Context, st *store.Store, cfg Config, target string, ids []string, section string) (Outcome, error) {
	if !cfg.Curation.Owner {
		return Outcome{}, fmt.Errorf("this machine is not the curation owner (curation.owner in config.yaml); nothing merged")
	}
	if st.Commit != nil && st.Commit.Busy() {
		return Outcome{}, fmt.Errorf("the repository is in the middle of a merge or rebase; nothing merged")
	}
	batches, _, _, err := Plan(st, "")
	if err != nil {
		return Outcome{}, err
	}
	for _, b := range batches {
		if b.Target == target && sameIDs(b.IDs, ids) {
			return runBatch(ctx, st, sectionWriter(section), b)
		}
	}
	return Outcome{}, fmt.Errorf("no ready batch for %s with these notes: it was merged already, or the notes changed; ask for the pending batches again", target)
}

type sectionWriter string

func (s sectionWriter) Section(context.Context, string, []gate.Note) (string, error) {
	return string(s), nil
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func runBatch(ctx context.Context, st *store.Store, m Merger, b Batch) (Outcome, error) {
	o := Outcome{Batch: b}
	notes, err := batchNotes(st, b)
	if err != nil {
		return o, err
	}
	if m == nil && !b.Missing {
		o.Result = "needs-writer"
		o.Problems = []string{"no model is configured (HMS_LLM_URL, HMS_LLM_MODEL); a harness can write it: hms curate -pending, then -submit"}
		return o, nil
	}
	stamp := store.Stamp()
	var section, header string
	if b.Missing {
		// a new entry: no model call, a scaffold of bullets, still checked
		header = "# " + b.Prefix + "\n\n"
		section = "## Consolidated learnings\n\n" + Fallback(notes)
		if ok, probs := gate.CheckAppend(notes, section); !ok {
			o.Problems = probs
			return o, fmt.Errorf("the scaffold for %s failed its own check: %v", b.Target, probs)
		}
		o.Result = "scaffold"
	} else {
		existing, err := st.WikiRead(b.Target)
		if err != nil {
			return o, err
		}
		body, merr := m.Section(ctx, existing, notes)
		body = cleanSection(body)
		ok, probs := false, []string(nil)
		if merr != nil {
			probs = []string{"model: " + merr.Error()}
		} else {
			ok, probs = gate.CheckAppend(notes, body)
		}
		if ok {
			o.Result = "merged"
		} else {
			o.Result, o.Problems = "fallback", probs
			body = Fallback(notes)
		}
		section = "## Consolidated learnings (" + stamp + ")\n\n" + body
	}
	if err := st.AppendSection(ctx, b.Target, header, section, b.IDs); err != nil {
		return o, err
	}
	return o, nil
}

var headingLine = regexp.MustCompile(`(?m)^#{1,6}\s.*$`)

// cleanSection removes code fences around the whole answer and any heading the model added: the section gets its own.
func cleanSection(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	return strings.TrimSpace(headingLine.ReplaceAllString(s, ""))
}

// Text renders a report for people.
func (r Report) Text() string {
	var b strings.Builder
	mode := "DRY RUN"
	if r.Applied {
		mode = "APPLIED"
	}
	fmt.Fprintf(&b, "%s (curation owner: %v)\n", mode, r.Owner)
	if r.Skipped != "" {
		fmt.Fprintf(&b, "%s\n", r.Skipped)
	}
	for _, o := range r.Outcomes {
		state := ""
		if o.Missing {
			state = ", entry is new"
		}
		fmt.Fprintf(&b, "%-9s %d notes -> %s (%s%s)\n", o.Result, len(o.IDs), o.Target, o.Project, state)
		for _, p := range o.Problems {
			fmt.Fprintf(&b, "          check: %s\n", p)
		}
	}
	for _, x := range r.Retired {
		fmt.Fprintf(&b, "retired   %s\n", x)
	}
	fmt.Fprintf(&b, "%d note(s) merged; %d waiting for a batch of %d; %d without a usable kind prefix (never merged automatically)\n", r.Merged(), r.Waiting, BatchSize, r.Manual)
	return b.String()
}
