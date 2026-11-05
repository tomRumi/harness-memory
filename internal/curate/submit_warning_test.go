package curate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSubmitWarnsWhenANewPageCopiesAnother(t *testing.T) {
	st, _, _ := meaningStore(t)
	ctx := context.Background()
	notes := learnN(t, st, proj, 2, tmplWorker)
	body := "# Workers\n\n" + bullets(notes)
	putFile(t, st, "wiki/proj/Workers.md", body)
	other := learnN(t, st, proj, 2, "Invoice %d totals %d euros before the end of the month.")

	rep, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{
		{Title: "Worker restarts", Summary: "s", Body: body, Notes: ids(notes)},
		{Title: "Billing", Summary: "s", Body: "# Billing\n\n" + bullets(other), Notes: ids(other)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 4 || len(rep.Warnings) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	w := rep.Warnings[0]
	if !strings.HasPrefix(w, `page "Worker restarts" looks like a copy of [[Workers]] (`) || !strings.HasSuffix(w, "); consider merging them") {
		t.Fatalf("warning %q", w)
	}
	if !strings.Contains(rep.String(), "warning: "+w+"\n") {
		t.Fatalf("text report:\n%s", rep)
	}
	raw, _ := json.Marshal(rep)
	var back struct{ Warnings []string }
	if json.Unmarshal(raw, &back) != nil || len(back.Warnings) != 1 || back.Warnings[0] != w {
		t.Fatalf("json: %s", raw)
	}
	if !exists(st, "wiki/proj/Worker restarts.md") {
		t.Fatal("a warning must not stop the write")
	}
}

func TestSubmitNoWarningForAChangedPageOrWithoutVectors(t *testing.T) {
	st, _, _ := meaningStore(t)
	ctx := context.Background()
	notes := learnN(t, st, proj, 2, tmplWorker)
	body := "# Workers\n\n" + bullets(notes)
	old := "# Workers\n\nOld text.\n"
	putFile(t, st, "wiki/proj/Workers.md", old)
	putFile(t, st, "wiki/proj/Workers copy.md", body) // a copy already exists, but this submit only changes Workers
	rep, err := Submit(ctx, st, Submission{Project: proj, Pages: []Page{{Title: "Workers", Summary: "s", Body: old + bullets(notes), Notes: ids(notes), BaseHash: Hash([]byte(old))}}})
	if err != nil || rep.Merged != 2 || len(rep.Warnings) != 0 {
		t.Fatalf("changing an existing page gives no warning: %v %+v", err, rep)
	}
	// no embedder: no warning, no failure
	st.Emb = nil
	more := learnN(t, st, proj, 1, "A single observation number %d about workers %d.")
	rep, err = Submit(ctx, st, Submission{Project: proj, Pages: []Page{{Title: "Workers again", Summary: "s", Body: "# Workers\n\n" + bullets(more), Notes: ids(more)}}})
	if err != nil || rep.Merged != 1 || len(rep.Warnings) != 0 || rep.Warnings == nil {
		t.Fatalf("%v %+v", err, rep)
	}
}
