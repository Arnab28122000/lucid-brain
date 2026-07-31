// Package llm is the memory service's narrow view of a language model: one
// JSON-returning completion call. Everything richer — routing, fallbacks, budget
// caps, prompt caching — lives in llm-gateway, and this package talks to it over
// the OpenAI-compatible surface that gateway exposes.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Request is a single structured-output completion.
type Request struct {
	System      string
	User        string
	MaxTokens   int
	Temperature float64
	// CacheKey lets llm-gateway reuse a prompt prefix. Extraction prompts are
	// long and near-identical across episodes, so this is most of the cost.
	CacheKey string
}

// Client is the seam the pipeline depends on; the fake in this package is what
// the extraction tests run against.
type Client interface {
	Complete(ctx context.Context, req Request) (string, error)
}

// ErrUnavailable means the model could not be reached after retries. The
// pipeline treats it as retryable and nacks the message rather than acking a
// half-extracted episode.
var ErrUnavailable = errors.New("llm: unavailable")

// HTTPClient talks to llm-gateway (or any OpenAI-compatible endpoint).
type HTTPClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTP       *http.Client
	MaxRetries int
}

// NewHTTP builds a client with timeouts sized for extraction, not for chat: the
// broad pass over a 10K-character chunk is allowed to be slow because nothing
// on the hot path is waiting for it.
func NewHTTP(baseURL, apiKey, model string) *HTTPClient {
	return &HTTPClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		HTTP:       &http.Client{Timeout: 120 * time.Second},
		MaxRetries: 3,
	}
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	Temperature    float64       `json:"temperature"`
	ResponseFormat *respFormat   `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type respFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete issues the request, retrying transient failures with linear backoff.
// Non-2xx below 500 is not retried — a 400 will be a 400 again.
func (c *HTTPClient) Complete(ctx context.Context, req Request) (string, error) {
	temp := req.Temperature
	body := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		MaxTokens:      req.MaxTokens,
		Temperature:    temp,
		ResponseFormat: &respFormat{Type: "json_object"},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("llm: marshal: %w", err)
	}

	attempts := c.MaxRetries
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		out, retryable, err := c.do(ctx, payload, req.CacheKey)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
}

func (c *HTTPClient) do(ctx context.Context, payload []byte, cacheKey string) (string, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("llm: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if cacheKey != "" {
		httpReq.Header.Set("X-Cortex-Prompt-Cache-Key", cacheKey)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", true, fmt.Errorf("llm: post: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", true, fmt.Errorf("llm: read: %w", err)
	}
	if resp.StatusCode >= 300 {
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return "", retryable, fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("llm: decode: %w", err)
	}
	if out.Error != nil {
		return "", false, fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", true, errors.New("llm: empty choices")
	}
	return out.Choices[0].Message.Content, false, nil
}

// ExtractJSON pulls the first JSON object out of a completion. Models wrap JSON
// in prose or fences often enough that failing the whole extraction over it
// would be a self-inflicted wound.
func ExtractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if fence := strings.Index(s, "```"); fence >= 0 {
		rest := s[fence+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			s = strings.TrimSpace(rest[:end])
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return "", errors.New("llm: no JSON in response")
	}
	open := s[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case ch == '\\' && inStr:
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
			// literal text; brackets inside strings must not move depth
		case ch == open:
			depth++
		case ch == closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", errors.New("llm: unterminated JSON in response")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Fake is a scripted client for tests and for the docker-compose evaluation
// mode, where an LLM may not be wired up at all.
type Fake struct {
	// Responses are returned in order; the last one repeats once exhausted.
	Responses []string
	Err       error
	Calls     []Request
}

func (f *Fake) Complete(_ context.Context, req Request) (string, error) {
	f.Calls = append(f.Calls, req)
	if f.Err != nil {
		return "", f.Err
	}
	if len(f.Responses) == 0 {
		return "{}", nil
	}
	idx := len(f.Calls) - 1
	if idx >= len(f.Responses) {
		idx = len(f.Responses) - 1
	}
	return f.Responses[idx], nil
}
