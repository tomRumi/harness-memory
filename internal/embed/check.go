package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultModel is the model used when HMS_EMBED_MODEL is not set.
const DefaultModel = "nomic-embed-text"

// ModelSize says how big a model download is. Only the default model is known; for any other the server states the
// size when the download starts.
func ModelSize(model string) string {
	if model == DefaultModel {
		return "about 274 MB"
	}
	return "size shown by the server when the download starts"
}

// State is what Check found.
type State int

const (
	// Ready: the server answers and the model embeds text.
	Ready State = iota
	// ModelMissing: an Ollama-style server answers but does not have the model.
	ModelMissing
	// Unreachable: no server, or the model does not embed (Err says why).
	Unreachable
)

// Status is the result of Check.
type Status struct {
	State State
	Dim   int   // vector size, when Ready
	Err   error // why, when Unreachable
}

// Config returns the configuration of the client.
func (c *Client) Config() Config { return c.cfg }

// Check looks at the embedder without changing anything. For the ollama API it asks the server which models it has
// (GET /api/tags); a server that answers without the model is ModelMissing. For the openai API, and for a server that
// does not answer /api/tags, a test embedding decides.
func (c *Client) Check(ctx context.Context) Status {
	if c.cfg.API == "ollama" {
		have, err := c.tags(ctx)
		if err == nil && !hasModel(have, c.cfg.Model) {
			return Status{State: ModelMissing}
		}
	}
	dim, err := c.Health(ctx)
	if err != nil {
		return Status{State: Unreachable, Err: err}
	}
	return Status{State: Ready, Dim: dim}
}

// tags lists the model names an Ollama-style server has.
func (c *Client) tags(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/api/tags answered %d", resp.StatusCode)
	}
	var r struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range r.Models {
		out = append(out, m.Name, m.Model)
	}
	return out, nil
}

// hasModel matches "name" with "name:latest", as Ollama does.
func hasModel(have []string, want string) bool {
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, h := range have {
		if !strings.Contains(h, ":") {
			h += ":latest"
		}
		if h == want {
			return true
		}
	}
	return false
}

// Pull asks an Ollama-style server to download the model (POST /api/pull, no streaming) and waits until it is done.
func (c *Client) Pull(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{"model": c.cfg.Model, "stream": false})
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.URL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 2 * time.Hour}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	json.Unmarshal(raw, &r)
	if resp.StatusCode != 200 || r.Error != "" {
		msg := r.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("pull answered %d: %s", resp.StatusCode, msg)
	}
	return nil
}
