package main

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
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hmsbin")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "hms")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func fakeOllama(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"nomic-embed-text:latest"}]}`)
		case "/api/embed":
			var in struct{ Input []string }
			json.NewDecoder(r.Body).Decode(&in)
			out := make([][]float32, len(in.Input))
			for i := range out {
				out[i] = []float32{1, 0, 0, 0}
			}
			json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func deadURL() string {
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()
	return u
}

// hms runs the program against a throwaway home and returns standard output, standard error and the exit status.
func hms(t *testing.T, home, url string, extraEnv []string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "HMS_HOME="+home, "HMS_EMBED_URL="+url, "HMS_VECTORS=", "HMS_EMBED_API=", "HMS_EMBED_MODEL=", "HMS_REPO=", "HMS_DB=")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func seeded(t *testing.T, url string) string {
	home := t.TempDir()
	if _, _, c := hms(t, home, url, nil, "", "init"); c != 0 {
		t.Fatal("init failed")
	}
	os.WriteFile(filepath.Join(home, "memory", "wiki", "ports.md"), []byte("# Ports\n\n## Daemon\nThe daemon listens on port 8765 over loopback only.\n"), 0o644)
	if _, _, c := hms(t, home, url, nil, "", "rescan"); c != 0 {
		t.Fatal("rescan failed")
	}
	return home
}

func TestRecallHeaderOnceWhenEmbedderDownAndNotWhenUp(t *testing.T) {
	up := fakeOllama(t)
	home := seeded(t, up.URL)
	if _, _, c := hms(t, home, up.URL, nil, "", "embed"); c != 0 {
		t.Fatal("embed failed")
	}
	out, _, c := hms(t, home, up.URL, nil, "", "recall", "daemon", "port")
	if c != 0 || strings.Contains(out, "vectors unavailable") || !strings.Contains(out, "8765") {
		t.Fatalf("embedder up: %d\n%s", c, out)
	}
	out, _, c = hms(t, home, deadURL(), nil, "", "recall", "daemon", "port")
	lines := strings.Split(out, "\n")
	if c != 0 || strings.Count(out, "vectors unavailable") != 1 || !strings.HasPrefix(lines[0], "[vectors unavailable: ") || !strings.HasSuffix(lines[0], "; keyword match only]") || !strings.Contains(out, "8765") {
		t.Fatalf("embedder down: %d\n%s", c, out)
	}
	out, _, _ = hms(t, home, deadURL(), []string{"HMS_VECTORS=0"}, "", "recall", "daemon", "port")
	if strings.Contains(out, "vectors unavailable") {
		t.Fatalf("HMS_VECTORS=0 must be plain keyword recall:\n%s", out)
	}
	out, _, _ = hms(t, home, deadURL(), nil, "", "recall", "-vectors=false", "daemon", "port")
	if strings.Contains(out, "vectors unavailable") {
		t.Fatalf("-vectors=false must be plain keyword recall:\n%s", out)
	}
}

func TestHookModeKeepsTheWarningOutOfTheBlock(t *testing.T) {
	home := seeded(t, deadURL())
	dead := deadURL()
	out, errOut, c := hms(t, home, dead, nil, `{"prompt":"daemon port loopback"}`, "recall", "-format=context", "-hook")
	if c != 0 || !strings.Contains(out, "8765") || strings.Contains(out, "vectors unavailable") || !strings.Contains(errOut, "vectors unavailable") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", c, out, errOut)
	}
	out, _, c = hms(t, home, dead, nil, `{"prompt":"zzzqqq nothing matches this"}`, "recall", "-format=context", "-hook")
	if c != 0 || out != "" {
		t.Fatalf("nothing matching must print nothing: %d %q", c, out)
	}
}

func TestStatsVectorLines(t *testing.T) {
	up := fakeOllama(t)
	home := seeded(t, up.URL)
	hms(t, home, up.URL, nil, "", "embed")
	out, _, _ := hms(t, home, up.URL, nil, "", "stats")
	if !strings.Contains(out, "vectors: on (nomic-embed-text), ") || !strings.Contains(out, " chunks embedded") {
		t.Fatal(out)
	}
	out, _, _ = hms(t, home, up.URL, []string{"HMS_VECTORS=0"}, "", "stats")
	if !strings.Contains(out, "vectors: off") {
		t.Fatal(out)
	}
	out, _, _ = hms(t, home, deadURL(), nil, "", "stats")
	if !strings.Contains(out, "vectors: unavailable: ") || !strings.Contains(out, "(keyword match only)") {
		t.Fatal(out)
	}
}

func TestDoctorExitStatus(t *testing.T) {
	up := fakeOllama(t)
	home := seeded(t, up.URL)
	if out, _, c := hms(t, home, up.URL, nil, "", "doctor"); c != 0 || strings.Contains(out, "FAIL") {
		t.Fatalf("%d\n%s", c, out)
	}
	if out, _, c := hms(t, home, deadURL(), nil, "", "doctor"); c != 0 || !strings.Contains(out, "WARN") {
		t.Fatalf("embedder down must be exit 0 with a WARN: %d\n%s", c, out)
	}
	os.RemoveAll(filepath.Join(home, "memory", ".git"))
	if out, _, c := hms(t, home, up.URL, nil, "", "doctor"); c != 1 || !strings.Contains(out, "FAIL") {
		t.Fatalf("%d\n%s", c, out)
	}
}

func TestInitFlagAndTranscript(t *testing.T) {
	home := t.TempDir()
	dead := deadURL()
	out, _, c := hms(t, home, dead, nil, "", "init", "--yes")
	if c != 0 || !strings.Contains(out, "ollama pull nomic-embed-text") {
		t.Fatalf("%d\n%s", c, out)
	}
	out2, _, c := hms(t, home, dead, nil, "", "init")
	if c != 0 || out2 != out {
		t.Fatalf("init twice must give the same output and exit 0")
	}
}
