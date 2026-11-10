package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tomRumi/harness-memory/internal/store"
)

type listed struct {
	Project string `json:"project"`
	Total   int    `json:"total"`
	Notes   []struct {
		ID, Short, Kind, Status, Author, Created, Text string
	} `json:"notes"`
}

func parseList(t *testing.T, out string) listed {
	t.Helper()
	var l listed
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return l
}

func TestListMemoryPagesFiltersAndOrder(t *testing.T) {
	s, srv, _, _, _ := setup(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		id, err := srv.St.LearnAs(ctx, fmt.Sprintf("List note number %d.", i), "p", "k:t", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		time.Sleep(2 * time.Millisecond)
	}
	// 5 notes, newest first, pages of 2 at offsets 0, 2, 4
	var got []string
	for _, off := range []int{0, 2, 4} {
		out, isErr := call(t, s, "list_memory", map[string]any{"project": "p", "limit": 2, "offset": off})
		if isErr {
			t.Fatal(out)
		}
		l := parseList(t, out)
		if l.Project != "p" || l.Total != 5 {
			t.Fatalf("offset %d: %+v", off, l)
		}
		for _, n := range l.Notes {
			got = append(got, strings.TrimPrefix(n.ID, "notes/p/"))
			if n.Short != n.ID[len("notes/p/"):][:8] || n.Kind != "k:t" || n.Status != "active" || !strings.HasPrefix(n.Text, "List note number") || n.Created == "" {
				t.Fatalf("note: %+v", n)
			}
		}
		if want := []int{2, 2, 1}[off/2]; len(l.Notes) != want {
			t.Fatalf("offset %d: %d notes, want %d", off, len(l.Notes), want)
		}
	}
	for i, id := range got {
		if id != ids[4-i] {
			t.Fatalf("not newest first: got %v, want reverse of %v", got, ids)
		}
	}
	// beyond the end: empty page, true total
	out, _ := call(t, s, "list_memory", map[string]any{"project": "p", "offset": 9})
	if l := parseList(t, out); l.Total != 5 || len(l.Notes) != 0 {
		t.Fatalf("past the end: %s", out)
	}
	// the status filter
	if _, err := srv.St.LearnAs(ctx, "A staged list note.", "p", "", "", &store.Writer{Author: "scout", Staged: true}); err != nil {
		t.Fatal(err)
	}
	out, _ = call(t, s, "list_memory", map[string]any{"project": "p", "status": "unverified"})
	l := parseList(t, out)
	if l.Total != 1 || len(l.Notes) != 1 || l.Notes[0].Status != "unverified" || l.Notes[0].Author != "scout" || l.Notes[0].Text != "A staged list note." {
		t.Fatalf("unverified: %s", out)
	}
	out, _ = call(t, s, "list_memory", map[string]any{"project": "p", "status": "active"})
	if l = parseList(t, out); l.Total != 5 || len(l.Notes) != 5 {
		t.Fatalf("active: %s", out)
	}
	out, _ = call(t, s, "list_memory", map[string]any{"project": "p"})
	if l = parseList(t, out); l.Total != 6 {
		t.Fatalf("all: %s", out)
	}
}

func TestListMemoryRefusesBadInput(t *testing.T) {
	s, _, _, _, _ := setup(t)
	for name, args := range map[string]map[string]any{
		"bad status":      {"project": "p", "status": "done"},
		"negative offset": {"project": "p", "offset": -1},
		"negative limit":  {"project": "p", "limit": -1},
		"empty project":   {"project": ""},
		"no project":      {},
	} {
		out, isErr := call(t, s, "list_memory", args)
		if !isErr {
			t.Fatalf("%s: not refused: %s", name, out)
		}
		if name == "bad status" && (!strings.Contains(out, "unverified") || !strings.Contains(out, "active")) {
			t.Fatalf("the refusal must name the allowed values: %s", out)
		}
	}
}

func TestListMemoryCutsLimitTo200(t *testing.T) {
	s, srv, _, _, _ := setup(t)
	ctx := context.Background()
	for i := 0; i < 203; i++ {
		if _, err := srv.St.LearnAs(ctx, fmt.Sprintf("Bulk note %d.", i), "p", "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := call(t, s, "list_memory", map[string]any{"project": "p", "limit": 5000})
	if l := parseList(t, out); l.Total != 203 || len(l.Notes) != 200 {
		t.Fatalf("total %d, page %d", l.Total, len(l.Notes))
	}
	out, _ = call(t, s, "list_memory", map[string]any{"project": "p"})
	if l := parseList(t, out); len(l.Notes) != 50 {
		t.Fatalf("default page %d", len(l.Notes))
	}
}

func TestListMemoryInTokenModeStaysInTheTokensProject(t *testing.T) {
	x := tokenSetup(t)
	for _, tok := range []string{"ra", "ta"} { // reading needs no write right
		out, isErr := call(t, x.client(t, tok), "list_memory", map[string]any{})
		if isErr {
			t.Fatalf("%s: %s", tok, out)
		}
		l := parseList(t, out)
		if l.Project != "a" || l.Total != 1 || strings.Contains(out, "of b") || strings.Contains(out, "global") {
			t.Fatalf("%s: %s", tok, out)
		}
	}
	if out, isErr := call(t, x.client(t, "ta"), "list_memory", map[string]any{"project": "b"}); !isErr || strings.Contains(out, "of b") {
		t.Fatalf("project b must be refused: %v %s", isErr, out)
	}
	out, _ := call(t, x.client(t, "tab"), "list_memory", map[string]any{"project": "b"})
	if l := parseList(t, out); l.Project != "b" || l.Total != 1 || !strings.Contains(out, "Zebra note of b.") {
		t.Fatalf("tab on b: %s", out)
	}
}
