package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomRumi/harness-memory/internal/md"
)

// A staged retire reads the status from the note's file, which a git pull may have changed since the last rescan.
func TestStagedRetireReadsStatusFromFile(t *testing.T) {
	st, _, repo := newStore(t)
	ctx := context.Background()
	id, _ := st.LearnAs(ctx, "A staged note verified elsewhere.", "p", "", "", &Writer{Author: "s", Staged: true})
	id = id[:36]
	rel := filepath.Join(repo, "notes", "p", id+".md")
	raw, _ := os.ReadFile(rel)
	data, _ := md.DeleteKey(raw, "status")
	os.WriteFile(rel, data, 0o644)
	if _, err := st.RetireAs(ctx, id[:8], "", "p", &Writer{Author: "s", Staged: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rel); err != nil {
		t.Errorf("a staged retire deleted a note that is active in its file")
	}
}

// Two notes that differ only after the cut at MaxNote are two notes, and the answer says the text was cut.
func TestLongNotesDifferingAfterTheCutAreTwoNotes(t *testing.T) {
	st, _, _ := newStore(t)
	ctx := context.Background()
	pre := strings.Repeat("word ", 100)
	a, _ := st.Learn(ctx, pre+"ENDING ONE: port 8080.", "p", "", "")
	b, _ := st.Learn(ctx, pre+"ENDING TWO: port 9090.", "p", "", "")
	if a[:36] == b[:36] {
		t.Errorf("the second note was dropped: %q %q", a, b)
	}
	if !strings.Contains(a, "cut to 500 characters") {
		t.Errorf("answer does not say the note was cut: %q", a)
	}
}
