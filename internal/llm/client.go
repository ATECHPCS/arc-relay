// Package llm provides a minimal OpenAI chat-completions client for tool
// optimization and other internal LLM tasks.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultBaseURL = "https://api.openai.com/v1/chat/completions"
	defaultModel   = "gpt-4o-mini"
	maxTokens      = 16384
)

// Client is a minimal OpenAI chat-completions client.
type Client struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewClient creates a new OpenAI client.
// If model is empty, defaults to gpt-4o-mini.
func NewClient(apiKey, model string) *Client {
	if model == "" {
		model = defaultModel
	}
	return &Client{
		apiKey:  apiKey,
		model:   model,
		baseURL: defaultBaseURL,
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// Available returns true if the client has an API key configured.
func (c *Client) Available() bool { return c.apiKey != "" }

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []Message `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Result holds the API response text and token usage.
type Result struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// Complete sends system + user messages to OpenAI and returns the response.
func (c *Client) Complete(ctx context.Context, system, userPrompt string) (*Result, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("LLM API key not configured (set ARC_RELAY_LLM_API_KEY)")
	}

	reqBody := chatRequest{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages: []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: userPrompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Error.Message != "" {
			return nil, fmt.Errorf("%s", apiErr.Error.Message)
		}
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result chatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("API error: %s: %s", result.Error.Type, result.Error.Message)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("API returned no choices")
	}

	return &Result{
		Text:         result.Choices[0].Message.Content,
		InputTokens:  result.Usage.PromptTokens,
		OutputTokens: result.Usage.CompletionTokens,
	}, nil
}
