package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComplete_SendsOpenAIShape(t *testing.T) {
	var captured struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "hello world"}}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 3}
		}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", "gpt-4o-mini")
	c.baseURL = srv.URL

	res, err := c.Complete(context.Background(), "you are helpful", "say hi")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q, want %q", res.Text, "hello world")
	}
	if res.InputTokens != 12 || res.OutputTokens != 3 {
		t.Errorf("tokens: in=%d out=%d, want 12/3", res.InputTokens, res.OutputTokens)
	}
	if capturedAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", capturedAuth)
	}
	if captured.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", captured.Model)
	}
	if len(captured.Messages) != 2 {
		t.Fatalf("expected 2 messages (system+user), got %d", len(captured.Messages))
	}
	if captured.Messages[0].Role != "system" || captured.Messages[0].Content != "you are helpful" {
		t.Errorf("first message wrong: %+v", captured.Messages[0])
	}
	if captured.Messages[1].Role != "user" || captured.Messages[1].Content != "say hi" {
		t.Errorf("second message wrong: %+v", captured.Messages[1])
	}
}

func TestComplete_NoAPIKey(t *testing.T) {
	c := NewClient("", "")
	_, err := c.Complete(context.Background(), "", "hi")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("expected API key error, got %v", err)
	}
}

func TestComplete_OpenAIErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": {"type": "invalid_api_key", "message": "bad key"}}`))
	}))
	defer srv.Close()
	c := NewClient("bad", "")
	c.baseURL = srv.URL
	_, err := c.Complete(context.Background(), "", "hi")
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("expected 'bad key' in error, got %v", err)
	}
}

func TestAvailable(t *testing.T) {
	if NewClient("", "").Available() {
		t.Error("Available() = true with empty key")
	}
	if !NewClient("k", "").Available() {
		t.Error("Available() = false with key")
	}
}

func TestDefaultModel(t *testing.T) {
	c := NewClient("k", "")
	if c.Model() != "gpt-4o-mini" {
		t.Errorf("default model = %q, want gpt-4o-mini", c.Model())
	}
}
