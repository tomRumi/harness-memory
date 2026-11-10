package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tomRumi/harness-memory/internal/access"
)

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func tokenFile(t *testing.T, dir, project string, toks map[string]string) string {
	t.Helper()
	var parts []string
	for tok, rights := range toks {
		parts = append(parts, `"`+access.Digest(tok)+`":{`+rights+`}`)
	}
	p := filepath.Join(dir, project+".json")
	body := `{"version":1,"project":"` + project + `","tokens":{` + strings.Join(parts, ",") + `}}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenSetup serves the memory in token mode with projects a and b. Token "ta" reads and writes a; "ra" only reads a;
// "tg" reads a and global; "tab" is granted a and b.
type tenancy struct {
	srv   *Server
	url   string
	dir   string
	repo  string
	afile string
}

func tokenSetup(t *testing.T) *tenancy {
	t.Helper()
	_, srv, repo, _, _ := setup(t)
	for rel, body := range map[string]string{
		"wiki/a/Alpha.md":  "# Alpha\n\nZebra stripes in a. See [[Beta]] and [[b/Secret]].\n",
		"wiki/a/Beta.md":   "# Beta\n\nBeta of a links [[Alpha]].\n",
		"wiki/b/Secret.md": "# Secret\n\nZebra stripes in b. See [[a/Alpha]].\n",
		"wiki/b/Other.md":  "# Other\n\nOther b page.\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755)
		os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644)
	}
	if _, err := srv.DB.Rescan(repo); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, n := range []struct{ text, proj string }{{"Zebra note of a.", "a"}, {"Zebra note of b.", "b"}, {"Zebra note of global.", "global"}} {
		if _, err := srv.St.Learn(ctx, n.text, n.proj, "k", ""); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "tokens")
	os.Mkdir(dir, 0o755)
	af := tokenFile(t, dir, "a", map[string]string{"ta": `"write":true`, "ra": ``, "tg": ``, "tab": `"write":true`})
	tokenFile(t, dir, "b", map[string]string{"tab": `"write":true`})
	tokenFile(t, dir, "global", map[string]string{"tg": ``})
	srv.TokensDir = dir
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &tenancy{srv, ts.URL, dir, repo, af}
}

func (x *tenancy) client(t *testing.T, tok string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: x.url + "/mcp", HTTPClient: &http.Client{Transport: bearer{tok}}}, nil)
	if err != nil {
		t.Fatalf("connect with %q: %v", tok, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func status(t *testing.T, url, auth string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	b.Write(buf[:n])
	return resp.StatusCode, b.String()
}

func TestTokenModeRefusesMissingAndUnknownTokens(t *testing.T) {
	x := tokenSetup(t)
	if c, _ := status(t, x.url, ""); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c, body := status(t, x.url, "Bearer nope"); c != 401 || strings.Contains(body, "nope") {
		t.Fatalf("unknown token: %d %q", c, body)
	}
	if c, _ := status(t, x.url, "Bearer ta"); c == 401 {
		t.Fatalf("a known token must pass the gate")
	}
}

func TestTokenProjectScoping(t *testing.T) {
	x := tokenSetup(t)
	a := x.client(t, "ta")
	if out, isErr := call(t, a, "recall", map[string]any{"query": "zebra", "project": "b"}); !isErr || !strings.Contains(out, "may not use project b") {
		t.Fatalf("a token asking b: %v %q", isErr, out)
	}
	out, _ := call(t, a, "recall", map[string]any{"query": "zebra"})
	if !strings.Contains(out, "in a") && !strings.Contains(out, "of a") || strings.Contains(out, "of b") || strings.Contains(out, "in b") || strings.Contains(out, "of global") {
		t.Fatalf("recall for an a token:\n%s", out)
	}
	if out, _ := call(t, x.client(t, "tg"), "recall", map[string]any{"query": "zebra"}); strings.Contains(out, "global") == false {
		t.Fatalf("a token with global must see the global hit:\n%s", out)
	}
	if out, _ := call(t, x.client(t, "tg"), "recall", map[string]any{"query": "zebra"}); strings.Contains(out, "of b") {
		t.Fatalf("%s", out)
	}
	// several projects: must name one
	if out, isErr := call(t, x.client(t, "tab"), "recall", map[string]any{"query": "zebra"}); !isErr || !strings.Contains(out, "a, b") {
		t.Fatalf("%v %q", isErr, out)
	}
	// wiki_read
	if out, isErr := call(t, a, "wiki_read", map[string]any{"path": "b/Secret.md"}); !isErr || strings.Contains(out, "Zebra") {
		t.Fatalf("a token read a b page: %v %q", isErr, out)
	}
	if out, isErr := call(t, a, "wiki_read", map[string]any{"path": "Secret"}); !isErr || strings.Contains(out, "Zebra") {
		t.Fatalf("a token read a b page by title: %v %q", isErr, out)
	}
	out, isErr := call(t, a, "wiki_read", map[string]any{"path": "a/Alpha.md"})
	if isErr || !strings.Contains(out, "Zebra stripes in a") || strings.Contains(out[strings.Index(out, "\n---\n"):], "Secret") || !strings.Contains(out, "Links to: [[Beta]]") {
		t.Fatalf("link lines must list only granted pages: %v\n%s", isErr, out)
	}
	if out, _ := call(t, x.client(t, "tab"), "wiki_read", map[string]any{"path": "a/Alpha.md"}); !strings.Contains(out[strings.Index(out, "\n---\n"):], "[[b/Secret]]") {
		t.Fatalf("a token granted both sees the link:\n%s", out)
	}
	// stats
	if out, _ := call(t, a, "stats", nil); strings.Contains(out, "/b") {
		t.Fatalf("stats shows b:\n%s", out)
	}
	if out, isErr := call(t, a, "stats", map[string]any{"project": "b"}); !isErr {
		t.Fatalf("stats for b: %q", out)
	}
}

func TestTokenRetireFeedbackAndWrite(t *testing.T) {
	x := tokenSetup(t)
	ctx := context.Background()
	idB, _ := x.srv.St.Learn(ctx, "Another b note to protect.", "b", "k", "")
	short := idB[:8]
	a := x.client(t, "ta")
	if out, _ := call(t, a, "retire", map[string]any{"id": short}); strings.Contains(out, "deleted") {
		t.Fatalf("a token retired a b note: %q", out)
	}
	if out, _ := call(t, a, "retire", map[string]any{"id": short, "project": "b"}); strings.Contains(out, "deleted") {
		t.Fatalf("%q", out)
	}
	if out, _ := call(t, a, "feedback", map[string]any{"id": short, "rating": "helpful", "session": "s"}); strings.Contains(out, "recorded") {
		t.Fatalf("feedback reached b: %q", out)
	}
	if _, err := os.Stat(filepath.Join(x.repo, "notes", "b", idB+".md")); err != nil {
		t.Fatalf("b note is gone: %v", err)
	}
	// write right
	ra := x.client(t, "ra")
	for name, args := range map[string]map[string]any{
		"learn":    {"text": "A note from a reader."},
		"fact_add": {"text": "A fact from a reader."},
		"retire":   {"id": "abcd"},
	} {
		if out, isErr := call(t, ra, name, args); !isErr || !strings.Contains(out, "may not write") {
			t.Fatalf("%s without write: %v %q", name, isErr, out)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(x.repo, "notes", "a", "*.md")); len(m) != 1 {
		t.Fatalf("a refused write left a file: %v", m)
	}
	// a writer lands in its only project
	if id, isErr := call(t, a, "learn", map[string]any{"text": "A note from a writer."}); isErr || len(id) < 36 {
		t.Fatalf("%v %q", isErr, id)
	}
	if m, _ := filepath.Glob(filepath.Join(x.repo, "notes", "a", "*.md")); len(m) != 2 {
		t.Fatalf("writer note missing: %v", m)
	}
	if out, isErr := call(t, a, "learn", map[string]any{"text": "Into b.", "project": "b"}); !isErr {
		t.Fatalf("%q", out)
	}
}

func TestTokenRemovedFileRevokesAtOnce(t *testing.T) {
	x := tokenSetup(t)
	a := x.client(t, "ta")
	if _, isErr := call(t, a, "stats", nil); isErr {
		t.Fatal("stats failed")
	}
	os.Remove(x.afile)
	tokenFile(t, x.dir, "b", map[string]string{"tab": ``}) // keep the directory valid
	if out, isErr := call(t, a, "stats", nil); !isErr {
		t.Fatalf("a removed file must revoke the token: %q", out)
	}
}

func TestTokenFaultsAreRefusedWithTheirName(t *testing.T) {
	x := tokenSetup(t)
	a := x.client(t, "ta")
	os.WriteFile(filepath.Join(x.dir, "c.json"), []byte(`{"version":1,"project":"c","tokens":{"bad":{}}}`), 0o644)
	code, body := status(t, x.url, "Bearer ta")
	if code < 500 || !strings.Contains(body, "c.json") || strings.Contains(body, access.Digest("ta")) {
		t.Fatalf("%d %q", code, body)
	}
	if _, err := a.CallTool(context.Background(), &mcp.CallToolParams{Name: "stats"}); err == nil {
		t.Fatal("a fault in a file must refuse the call")
	}
	os.Remove(filepath.Join(x.dir, "c.json"))
	os.Chmod(x.dir, 0o777)
	if code, body := status(t, x.url, "Bearer ta"); code < 500 || !strings.Contains(body, x.dir) {
		t.Fatalf("%d %q", code, body)
	}
}
