package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaCallPrefixesAndNormalizes(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		n := len(got["input"].([]any))
		out := make([][]float32, n)
		for i := range out {
			out[i] = []float32{3, 4, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	}))
	defer srv.Close()
	c := New(Config{API: "ollama", URL: srv.URL, Model: "m", DocPrefix: "doc: ", QueryPrefix: "q: "})
	v, err := c.Docs(context.Background(), []string{"a", "b"})
	if err != nil || len(v) != 2 {
		t.Fatalf("%v %v", v, err)
	}
	if math.Abs(float64(v[0][0])-0.6) > 1e-6 || math.Abs(float64(v[0][1])-0.8) > 1e-6 {
		t.Fatalf("not normalized: %v", v[0])
	}
	if in := got["input"].([]any); in[0] != "doc: a" || in[1] != "doc: b" {
		t.Fatalf("prefix: %v", in)
	}
	if _, err := c.Query(context.Background(), "x"); err != nil || got["input"].([]any)[0] != "q: x" {
		t.Fatalf("query prefix: %v %v", got, err)
	}
}

func TestOpenAIShapeAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k-test" {
			t.Errorf("path %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"data":[{"embedding":[0,2]}]}`))
	}))
	defer srv.Close()
	c := New(Config{API: "openai", URL: srv.URL + "/v1", Model: "m", Key: "k-test"})
	if dim, err := c.Health(context.Background()); err != nil || dim != 2 {
		t.Fatalf("%d %v", dim, err)
	}
}

func TestErrorsAreSpecific(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model \"nope\" not found"}`, 404)
	}))
	defer bad.Close()
	if _, err := New(Config{API: "ollama", URL: bad.URL, Model: "nope"}).Health(context.Background()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("model error: %v", err)
	}
	if _, err := New(Config{API: "ollama", URL: "http://127.0.0.1:1", Model: "m"}).Health(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable: %v", err)
	}
	zero := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"embeddings":[[0,0]]}`)) }))
	defer zero.Close()
	if _, err := New(Config{API: "ollama", URL: zero.URL, Model: "m"}).Health(context.Background()); err == nil {
		t.Fatal("a zero vector must be an error")
	}
	if _, err := New(Config{API: "carrier-pigeon"}).Health(context.Background()); err == nil {
		t.Fatal("unknown API must be an error")
	}
}

func TestIDChangesWithTheDocumentPrefixAndOpenAIOrderIsRestored(t *testing.T) {
	a := New(Config{Model: "m", DocPrefix: "search_document: "})
	b := New(Config{Model: "m", DocPrefix: ""})
	c := New(Config{Model: "m", DocPrefix: "passage: "})
	if a.ID() == b.ID() || a.ID() == c.ID() || b.ID() != "m" {
		t.Fatalf("ids %q %q %q", a.ID(), b.ID(), c.ID())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":1,"embedding":[0,3]},{"index":0,"embedding":[4,0]}]}`)) // out of order
	}))
	defer srv.Close()
	v, err := New(Config{API: "openai", URL: srv.URL, Model: "m"}).Docs(context.Background(), []string{"first", "second"})
	if err != nil || v[0][0] != 1 || v[1][1] != 1 {
		t.Fatalf("order not restored: %v %v", v, err)
	}
}
