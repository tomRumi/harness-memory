package curate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// LLM calls any server that speaks the OpenAI chat-completions protocol (Ollama does, at /v1). It is optional: the
// normal path is that a harness writes the text with its own model. Settings come from the
// environment and there are no defaults: HMS_LLM_URL and HMS_LLM_MODEL must both be set, HMS_LLM_KEY only if the server
// needs one. A subscription login (OAuth) cannot be used here; that belongs to the harness.
type LLM struct {
	URL, Model, Key string
	HTTP            *http.Client
}

// LLMFromEnv returns the configured model, or nil when none is configured.
func LLMFromEnv() *LLM {
	url, model := os.Getenv("HMS_LLM_URL"), os.Getenv("HMS_LLM_MODEL")
	if url == "" || model == "" {
		return nil
	}
	return &LLM{URL: url, Model: model, Key: os.Getenv("HMS_LLM_KEY"), HTTP: &http.Client{Timeout: timeoutFromEnv()}}
}

// MaxTokens bounds the answer. Pages are returned whole, so it is large; a small model that does not stop would
// otherwise run until the timeout.
const MaxTokens = 6000

// timeoutFromEnv is HMS_LLM_TIMEOUT in seconds (default 180).
func timeoutFromEnv() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("HMS_LLM_TIMEOUT")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 180 * time.Second
}

// Complete sends one user message and returns the text of the answer.
func (l *LLM) Complete(ctx context.Context, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       l.Model,
		"temperature": 0.2,
		"max_tokens":  MaxTokens,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(l.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.Key != "" {
		req.Header.Set("Authorization", "Bearer "+l.Key)
	}
	cl := l.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("model server answered %d: %.200s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("model answer not understood: %.200s", raw)
	}
	return out.Choices[0].Message.Content, nil
}
