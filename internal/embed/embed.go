// Package embed talks to an embedding model server: a local Ollama or any OpenAI-compatible endpoint.
// No model is part of this repository; the server must have one.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config says where the model is and how its inputs are marked.
type Config struct {
	API         string // "ollama" or "openai"
	URL         string // base URL of the server
	Model       string
	Key         string // bearer key for the openai API; read from HMS_EMBED_KEY, never stored in a file
	DocPrefix   string // text put before a stored chunk (models such as nomic-embed-text expect one)
	QueryPrefix string // text put before a query
}

// Default is the configuration used when nothing is set: a local Ollama with nomic-embed-text.
// HMS_EMBED_API, HMS_EMBED_URL, HMS_EMBED_MODEL, HMS_EMBED_KEY, HMS_EMBED_DOC_PREFIX and HMS_EMBED_QUERY_PREFIX override it.
func Default() Config {
	c := Config{API: "ollama", URL: "http://localhost:11434", Model: "nomic-embed-text",
		DocPrefix: "search_document: ", QueryPrefix: "search_query: "}
	if v := os.Getenv("HMS_EMBED_API"); v != "" {
		c.API = v
	}
	if v := os.Getenv("HMS_EMBED_URL"); v != "" {
		c.URL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("HMS_EMBED_MODEL"); v != "" {
		c.Model = v
		c.DocPrefix, c.QueryPrefix = "", "" // a different model does not take nomic's prefixes unless they are set below
	}
	if v, ok := os.LookupEnv("HMS_EMBED_DOC_PREFIX"); ok {
		c.DocPrefix = v
	}
	if v, ok := os.LookupEnv("HMS_EMBED_QUERY_PREFIX"); ok {
		c.QueryPrefix = v
	}
	c.Key = os.Getenv("HMS_EMBED_KEY")
	return c
}

// Client embeds text. It satisfies index.Embedder.
type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client { return &Client{cfg: cfg, http: &http.Client{Timeout: 5 * time.Minute}} }

func (c *Client) Model() string { return c.cfg.Model }

// Docs embeds stored chunks. The vectors are unit length.
func (c *Client) Docs(ctx context.Context, texts []string) ([][]float32, error) {
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = c.cfg.DocPrefix + t
	}
	return c.call(ctx, in)
}

// Query embeds one query. The vector is unit length.
func (c *Client) Query(ctx context.Context, text string) ([]float32, error) {
	v, err := c.call(ctx, []string{c.cfg.QueryPrefix + text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

// Health embeds a short text and returns the vector size; a missing server or model is an error that says so.
func (c *Client) Health(ctx context.Context) (int, error) {
	v, err := c.Query(ctx, "health check")
	if err != nil {
		return 0, err
	}
	return len(v), nil
}

func (c *Client) call(ctx context.Context, input []string) ([][]float32, error) {
	var url string
	var body []byte
	switch c.cfg.API {
	case "ollama":
		url = c.cfg.URL + "/api/embed"
		body, _ = json.Marshal(map[string]any{"model": c.cfg.Model, "input": input})
	case "openai":
		url = strings.TrimSuffix(c.cfg.URL, "/v1") + "/v1/embeddings"
		body, _ = json.Marshal(map[string]any{"model": c.cfg.Model, "input": input})
	default:
		return nil, fmt.Errorf("embedder: unknown API %q (use ollama or openai)", c.cfg.API)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedder unreachable at %s: %w", c.cfg.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("embedder %s answered %d: %s", c.cfg.URL, resp.StatusCode, msg)
	}
	var out [][]float32
	if c.cfg.API == "ollama" {
		var r struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("embedder answer: %w", err)
		}
		out = r.Embeddings
	} else {
		var r struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("embedder answer: %w", err)
		}
		for _, d := range r.Data {
			out = append(out, d.Embedding)
		}
	}
	if len(out) != len(input) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d inputs", len(out), len(input))
	}
	for i := range out {
		if !Normalize(out[i]) {
			return nil, fmt.Errorf("embedder returned an empty or zero vector for input %d", i)
		}
	}
	return out, nil
}

// Normalize scales v to unit length in place. It reports false for an empty or zero vector.
func Normalize(v []float32) bool {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if len(v) == 0 || s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
		return false
	}
	n := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= n
	}
	return true
}
