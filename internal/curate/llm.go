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

	"github.com/tomRumi/harness-memory-service/internal/gate"
)

// LLM calls any server that speaks the OpenAI chat-completions protocol (Ollama does, at /v1). Settings come from the
// environment: HMS_LLM_URL (default http://localhost:11434/v1), HMS_LLM_MODEL (default granite4:3b, the predecessor's
// editor model), HMS_LLM_KEY (only if the server needs one).
type LLM struct {
	URL, Model, Key string
	HTTP            *http.Client
}

func LLMFromEnv() *LLM {
	get := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	return &LLM{URL: get("HMS_LLM_URL", "http://localhost:11434/v1"), Model: get("HMS_LLM_MODEL", "granite4:3b"), Key: os.Getenv("HMS_LLM_KEY"), HTTP: &http.Client{Timeout: timeoutFromEnv()}}
}

// MaxTokens bounds the answer: ten notes of up to 500 characters need about 2,000 tokens, and a small model that does
// not stop would otherwise run until the timeout.
const MaxTokens = 3000

// timeoutFromEnv is HMS_LLM_TIMEOUT in seconds (default 180). When it runs out the notes are appended as plain bullets.
func timeoutFromEnv() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("HMS_LLM_TIMEOUT")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 180 * time.Second
}

// Prompt is the predecessor's merge prompt, changed in one respect: the model writes one new section, it does not
// return the whole file.
const Prompt = `You are adding to a project wiki. Below is the CURRENT FILE and some NEW LEARNINGS, each with an id.
Write ONE new section that states every learning as natural prose or bullets in the voice of the file.
- Keep every number, name, path and command of a learning exactly as written.
- Every learning below ends with its id in double square brackets. Keep that citation at the end of the sentence or bullet that states the learning, copied exactly, for example "- The job runs every 5 minutes. [[3f2a9c1e-0000-0000-0000-000000000000]]".
- Do not repeat what the file already says, and do not remove or change anything in the file.
- Output only the section text: no heading, no commentary.

CURRENT FILE:
<<<FILE
%s
FILE>>>

NEW LEARNINGS:
%s

Output the new section now:`

func (l *LLM) Section(ctx context.Context, existing string, notes []gate.Note) (string, error) {
	var nl strings.Builder
	for _, n := range notes {
		fmt.Fprintf(&nl, "- %s [[%s]]\n", strings.Join(strings.Fields(n.Text), " "), n.ID)
	}
	body, _ := json.Marshal(map[string]any{
		"model":       l.Model,
		"temperature": 0.2,
		"max_tokens":  MaxTokens,
		"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf(Prompt, existing, nl.String())}},
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
