package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tomRumi/harness-memory/internal/embed"
	"github.com/tomRumi/harness-memory/internal/index"
)

// fakeOllama imitates GET /api/tags, POST /api/embed and POST /api/pull.
type fakeOllama struct {
	*httptest.Server
	hasModel atomic.Bool
	pulls    atomic.Int32
	dim      int
}

func newFake(t *testing.T, hasModel bool) *fakeOllama {
	f := &fakeOllama{dim: 8}
	f.hasModel.Store(hasModel)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		if f.hasModel.Load() {
			fmt.Fprint(w, `{"models":[{"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest"}]}`)
		} else {
			fmt.Fprint(w, `{"models":[{"name":"other:latest","model":"other:latest"}]}`)
		}
	})
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		if !f.hasModel.Load() {
			http.Error(w, `{"error":"model not found"}`, 404)
			return
		}
		var in struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&in)
		out := make([][]float32, len(in.Input))
		for i := range out {
			out[i] = make([]float32, f.dim)
			out[i][i%f.dim] = 1
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	})
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model  string
			Stream *bool
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Stream == nil || *in.Stream || in.Model != "nomic-embed-text" {
			http.Error(w, `{"error":"bad pull request"}`, 400)
			return
		}
		f.pulls.Add(1)
		f.hasModel.Store(true)
		fmt.Fprint(w, `{"status":"success"}`)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// openaiFake imitates POST /v1/embeddings.
func newOpenAIFake(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,0,0,0]}]}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func env(t *testing.T, url string) {
	t.Setenv("HMS_EMBED_API", "")
	t.Setenv("HMS_EMBED_MODEL", "")
	t.Setenv("HMS_EMBED_KEY", "")
	t.Setenv("HMS_VECTORS", "")
	t.Setenv("HMS_EMBED_URL", url)
}

func paths(t *testing.T) Paths {
	h := t.TempDir()
	return Paths{Home: h, Repo: filepath.Join(h, "memory"), DB: filepath.Join(h, "index.db")}
}

func runInit(t *testing.T, p Paths, o InitOptions) string {
	t.Helper()
	var b bytes.Buffer
	o.Out = &b
	if err := Init(p, o); err != nil {
		t.Fatalf("init failed: %v\n%s", err, b.String())
	}
	return b.String()
}

func TestInitModelPresent(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	out := runInit(t, p, InitOptions{})
	if !strings.Contains(out, "vectors: on (nomic-embed-text)") || f.pulls.Load() != 0 {
		t.Fatalf("%s", out)
	}
	for _, d := range []string{".git", "wiki", "notes", "facts", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(p.Repo, d)); err != nil {
			t.Fatalf("missing %s", d)
		}
	}
	if _, err := os.Stat(p.DB); err != nil {
		t.Fatal("no index")
	}
}

func TestInitModelMissingYesPullsOnce(t *testing.T) {
	f := newFake(t, false)
	env(t, f.URL)
	out := runInit(t, paths(t), InitOptions{Yes: true})
	if f.pulls.Load() != 1 || !strings.Contains(out, "about 274 MB") || !strings.Contains(out, "vectors: on (nomic-embed-text)") {
		t.Fatalf("pulls %d\n%s", f.pulls.Load(), out)
	}
}

func TestInitModelMissingAnswerYesAndNo(t *testing.T) {
	f := newFake(t, false)
	env(t, f.URL)
	out := runInit(t, paths(t), InitOptions{Interactive: true, In: strings.NewReader("n\n")})
	if f.pulls.Load() != 0 || !strings.Contains(out, "Download it now? [y/N]") || !strings.Contains(out, "ollama pull nomic-embed-text") {
		t.Fatalf("%s", out)
	}
	out = runInit(t, paths(t), InitOptions{Interactive: true, In: strings.NewReader("y\n")})
	if f.pulls.Load() != 1 || !strings.Contains(out, "vectors: on") {
		t.Fatalf("%s", out)
	}
}

func TestInitModelMissingNoTerminalNoYesDoesNotPull(t *testing.T) {
	f := newFake(t, false)
	env(t, f.URL)
	out := runInit(t, paths(t), InitOptions{Interactive: false, In: strings.NewReader("y\n")})
	if f.pulls.Load() != 0 || !strings.Contains(out, "run: ollama pull nomic-embed-text") || strings.Contains(out, "[y/N]") {
		t.Fatalf("pulls %d\n%s", f.pulls.Load(), out)
	}
}

func TestInitOtherModelSizeText(t *testing.T) {
	f := newFake(t, false)
	env(t, f.URL)
	t.Setenv("HMS_EMBED_MODEL", "some-other-model")
	out := runInit(t, paths(t), InitOptions{})
	if !strings.Contains(out, "some-other-model") || !strings.Contains(out, "size shown by the server when the download starts") || strings.Contains(out, "274 MB") {
		t.Fatalf("%s", out)
	}
}

func TestInitNoEmbedderOneLineAndSuccess(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	env(t, url)
	out := runInit(t, paths(t), InitOptions{})
	n := strings.Count(out, "install Ollama (https://ollama.com) and run: ollama pull nomic-embed-text")
	if n != 1 || !strings.Contains(out, "HMS_EMBED_URL") || strings.Contains(out, "vectors: on") {
		t.Fatalf("%s", out)
	}
}

func TestInitOpenAIChecksATestEmbeddingOnly(t *testing.T) {
	s := newOpenAIFake(t)
	env(t, s.URL)
	t.Setenv("HMS_EMBED_API", "openai")
	t.Setenv("HMS_EMBED_MODEL", "text-embed-x")
	out := runInit(t, paths(t), InitOptions{Yes: true})
	if !strings.Contains(out, "vectors: on (text-embed-x)") || strings.Contains(out, "download") {
		t.Fatalf("%s", out)
	}
}

func TestInitVectorsOff(t *testing.T) {
	f := newFake(t, false)
	env(t, f.URL)
	t.Setenv("HMS_VECTORS", "0")
	out := runInit(t, paths(t), InitOptions{Yes: true})
	if f.pulls.Load() != 0 || !strings.Contains(out, "vectors: off") {
		t.Fatalf("%s", out)
	}
}

func snapshot(t *testing.T, root string) string {
	var b strings.Builder
	filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || strings.Contains(path, "/.git/") && !strings.HasSuffix(path, "/HEAD") && !strings.HasSuffix(path, "/config") {
			return nil
		}
		if strings.HasSuffix(path, ".db") || strings.HasSuffix(path, "-vectors.db") {
			fmt.Fprintln(&b, path)
			return nil
		}
		data := ""
		if !fi.IsDir() {
			raw, _ := os.ReadFile(path)
			data = string(raw)
		}
		fmt.Fprintln(&b, path, fi.IsDir(), data)
		return nil
	})
	return b.String()
}

func TestInitTwiceChangesNothing(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	first := runInit(t, p, InitOptions{})
	s1 := snapshot(t, p.Home)
	second := runInit(t, p, InitOptions{})
	if first != second || s1 != snapshot(t, p.Home) {
		t.Fatalf("second init changed something:\n%s\n---\n%s", first, second)
	}
	// and with files already there
	os.WriteFile(filepath.Join(p.Repo, "notes", "x.md"), []byte("x"), 0o644)
	before := snapshot(t, p.Home)
	runInit(t, p, InitOptions{})
	if before != snapshot(t, p.Home) {
		t.Fatal("init touched existing files")
	}
}

func doctor(t *testing.T, p Paths) (string, int) {
	t.Helper()
	var b bytes.Buffer
	n := Doctor(p, &b)
	return b.String(), n
}

func TestDoctorAllFine(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	out, fails := doctor(t, p)
	if fails != 0 || strings.Contains(out, "WARN") || strings.Contains(out, "FAIL") || strings.Count(out, "OK") < 8 || !strings.Contains(out, "0 broken, 0 ambiguous") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
	for _, w := range []string{"git program", "memory repository", "index", "embedder", "model", "vector size", "chunks without vector", "commits", "links"} {
		if !strings.Contains(out, w) {
			t.Fatalf("no line for %q:\n%s", w, out)
		}
	}
}

func TestDoctorEmbedderDownIsAWarnNotAFail(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	f.Close()
	out, fails := doctor(t, p)
	if fails != 0 || !strings.Contains(out, "WARN  embedder") || strings.Contains(out, "FAIL") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
}

func TestDoctorModelMissingIsAWarn(t *testing.T) {
	f := newFake(t, false)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	out, fails := doctor(t, p)
	if fails != 0 || !strings.Contains(out, "WARN  model") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
}

func TestDoctorNotAGitRepoFails(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	os.RemoveAll(filepath.Join(p.Repo, ".git"))
	out, fails := doctor(t, p)
	if fails != 1 || !strings.Contains(out, "FAIL  memory repository") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
}

func TestDoctorSchemaMismatchFails(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	d, _ := index.Open(p.DB)
	d.Exec(`PRAGMA user_version = 1`)
	d.Close()
	out, fails := doctor(t, p)
	if fails != 1 || !strings.Contains(out, "FAIL  index") || !strings.Contains(out, "schema version 1") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
	d, _ = index.Open(p.DB) // doctor changed nothing: the old schema number was still there for it to see
	d.Close()
}

func TestDoctorChangesNothing(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	before := snapshot(t, p.Home)
	doctor(t, p)
	if before != snapshot(t, p.Home) {
		t.Fatal("doctor changed files")
	}
	// no index at all: doctor must not create one
	os.Remove(p.DB)
	out, fails := doctor(t, p)
	if _, err := os.Stat(p.DB); err == nil || fails != 1 || !strings.Contains(out, "FAIL  index") {
		t.Fatalf("doctor must not create the index (fails %d)\n%s", fails, out)
	}
}

func TestDoctorVectorSizeMismatchAndMissingVectors(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	os.WriteFile(filepath.Join(p.Repo, "wiki", "a.md"), []byte("# A\n\nSome words about daemons.\n"), 0o644)
	d, _ := index.Open(p.DB)
	d.Rescan(p.Repo)
	d.Close()
	out, fails := doctor(t, p)
	if fails != 0 || !strings.Contains(out, "WARN  chunks without vector") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
	id := embed.New(embed.Default()).ID()
	d, _ = index.Open(p.DB)
	if _, err := d.Exec(`INSERT INTO vec.vectors(text_hash, model, dim, vec) VALUES('h', ?, 3, x'000000000000000000000000')`, id); err != nil {
		t.Fatal(err)
	}
	d.Close()
	out, fails = doctor(t, p)
	if fails != 1 || !strings.Contains(out, "FAIL  vector size") || !strings.Contains(out, "gives 8, stored vectors have 3") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
	d, _ = index.Open(p.DB)
	d.Exec(`INSERT INTO vec.vectors(text_hash, model, dim, vec) VALUES('h2', ?, 4, x'00000000000000000000000000000000')`, id)
	d.Close()
	if out, fails = doctor(t, p); fails != 1 || !strings.Contains(out, "mixed sizes") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
}

func TestDoctorRepoStateForeignStaged(t *testing.T) {
	f := newFake(t, true)
	env(t, f.URL)
	p := paths(t)
	runInit(t, p, InitOptions{})
	os.WriteFile(filepath.Join(p.Repo, "other.txt"), []byte("x"), 0o644)
	if out, err := exec.Command("git", "-C", p.Repo, "add", "other.txt").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	out, fails := doctor(t, p)
	if fails != 0 || !strings.Contains(out, "WARN  commits") || !strings.Contains(out, "other.txt") {
		t.Fatalf("fails %d\n%s", fails, out)
	}
}
