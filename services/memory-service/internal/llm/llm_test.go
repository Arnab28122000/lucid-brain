package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare object", `{"a":1}`, `{"a":1}`},
		{"leading prose", `Sure! Here it is: {"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced without a language tag", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"trailing prose", `{"a":1} — hope that helps!`, `{"a":1}`},
		{"nested objects", `{"a":{"b":[1,2]}}`, `{"a":{"b":[1,2]}}`},
		{"array root", `[{"a":1}]`, `[{"a":1}]`},
		// A brace inside a string literal must not close the object; this is the
		// case a naive index-of-last-brace implementation gets wrong.
		{"braces inside strings", `{"quote":"he said {done} loudly"}`, `{"quote":"he said {done} loudly"}`},
		{"escaped quotes", `{"quote":"she said \"yes\""}`, `{"quote":"she said \"yes\""}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractJSON(tc.in)
			if err != nil {
				t.Fatalf("ExtractJSON(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ExtractJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("result is not valid JSON: %q", got)
			}
		})
	}
}

func TestExtractJSONErrors(t *testing.T) {
	for _, in := range []string{"", "I'm sorry, I can't help with that.", `{"a":1`} {
		if _, err := ExtractJSON(in); err == nil {
			t.Errorf("ExtractJSON(%q) = nil error, want an error", in)
		}
	}
}

func TestCompleteRetriesTransientFailures(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "", "test-model")
	got, err := c.Complete(context.Background(), Request{User: "hello"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != `{"ok":true}` {
		t.Errorf("content = %q", got)
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3 (two failures then success)", calls)
	}
}

func TestCompleteDoesNotRetryClientErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"context length exceeded"}}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "", "test-model")
	if _, err := c.Complete(context.Background(), Request{User: "hello"}); err == nil {
		t.Fatal("Complete succeeded on a 400")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1 — a 400 will be a 400 again", calls)
	}
}

func TestCompleteSendsThePromptCacheKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Cortex-Prompt-Cache-Key")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "", "test-model")
	if _, err := c.Complete(context.Background(), Request{User: "hi", CacheKey: "memory-broad-v1"}); err != nil {
		t.Fatal(err)
	}
	// Extraction prompts are long and near-identical across episodes, so the
	// gateway's prompt cache is most of the cost saving.
	if gotKey != "memory-broad-v1" {
		t.Errorf("cache key header = %q, want the request's cache key", gotKey)
	}
}

func TestCompleteRequestsJSONMode(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "", "test-model")
	if _, err := c.Complete(context.Background(), Request{System: "sys", User: "usr"}); err != nil {
		t.Fatal(err)
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want json_object", body["response_format"])
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want a system and a user message", body["messages"])
	}
}

func TestFakeReplaysScriptedResponsesInOrder(t *testing.T) {
	f := &Fake{Responses: []string{"first", "second"}}
	ctx := context.Background()

	for _, want := range []string{"first", "second", "second"} {
		got, err := f.Complete(ctx, Request{User: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if len(f.Calls) != 3 {
		t.Errorf("recorded %d calls, want 3", len(f.Calls))
	}
	if !strings.Contains(f.Calls[0].User, "x") {
		t.Error("the fake should record the request it was given")
	}
}
