package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// Item 16: /healthz has the loopback Host check of /mcp.
func TestHealthzRefusesAForeignHost(t *testing.T) {
	_, _, _, _, url := setup(t)
	for host, want := range map[string]int{
		"evil.example.test":      403,
		"evil.example.test:8765": 403,
		"10.0.0.5":               403,
		"localhost":              200,
		"localhost:8765":         200,
		"127.0.0.1:8765":         200,
		"[::1]:8765":             200,
	} {
		req, _ := http.NewRequest("GET", url+"/healthz", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %q: %d, want %d", host, resp.StatusCode, want)
		}
	}
	// and /mcp keeps its own check
	req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader("{}"))
	req.Host = "evil.example.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Errorf("/mcp foreign host: %v %v", resp, err)
	}
}

// Item 17: kind is optional in the schema and the description says what it is.
func TestLearnKindIsOptional(t *testing.T) {
	s, _, repo, _, _ := setup(t)
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name != "learn" {
			continue
		}
		raw, _ := json.Marshal(tl.InputSchema)
		var sch struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		json.Unmarshal(raw, &sch)
		for _, r := range sch.Required {
			if r == "kind" {
				t.Errorf("kind is required: %s", raw)
			}
		}
		if d := sch.Properties["kind"].Description; strings.Contains(d, "decides where") || !strings.Contains(d, "informational") {
			t.Errorf("kind description %q", d)
		}
		if !strings.Contains(tl.Description, "informational") {
			t.Errorf("tool description %q", tl.Description)
		}
	}
	out, isErr := call(t, s, "learn", map[string]any{"text": "A learning stored without a kind label at all.", "project": "proj"})
	if isErr {
		t.Fatalf("learn without kind: %s", out)
	}
	if b, _ := exec.Command("grep", "-rl", "kind: learning", repo+"/notes").Output(); len(b) == 0 {
		t.Error("default kind learning not written")
	}
}

// Item 18: a long recall query is cut, not refused.
func TestRecallQueryIsCutAt2000Characters(t *testing.T) {
	s, srv, _, _, _ := setup(t)
	q := strings.Repeat("daemon port ", 500) // 6,000 characters
	out, isErr := call(t, s, "recall", map[string]any{"query": q})
	if isErr || !strings.Contains(out, "8765") {
		t.Fatalf("%v %q", isErr, out)
	}
	lines, _, early := srv.recall(context.Background(), RecallIn{Query: strings.Repeat("a", 5000)})
	_ = lines
	if early != "" && strings.Contains(early, "too long") {
		t.Fatal(early)
	}
	if MaxQuery != 2000 {
		t.Fatal(MaxQuery)
	}
}

// Item 15: stats says so when HEAD is detached.
func TestStatsSaysWhenHeadIsDetached(t *testing.T) {
	_, srv, repo, _, _ := setup(t)
	for _, args := range [][]string{{"commit", "-q", "--allow-empty", "-m", "x"}, {"checkout", "-q", "--detach"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if out := srv.Stats(context.Background(), StatsIn{}); !strings.Contains(out, "commits: paused, HEAD is detached; check out a branch") {
		t.Fatalf("stats:\n%s", out)
	}
}
