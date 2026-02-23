package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// RemoteProxy forwards MCP requests to a remote MCP server with auth.
type RemoteProxy struct {
	config     store.RemoteConfig
	sessionID  string
	httpClient *http.Client
}

// NewRemoteProxy creates a proxy to a remote MCP server.
func NewRemoteProxy(config store.RemoteConfig) *RemoteProxy {
	return &RemoteProxy{
		config:     config,
		httpClient: &http.Client{},
	}
}

// Send forwards an MCP request to the remote server.
func (p *RemoteProxy) Send(ctx context.Context, req *mcp.Request) (*mcp.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.config.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if p.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", p.sessionID)
	}

	// Apply auth
	switch p.config.Auth.Type {
	case "bearer":
		httpReq.Header.Set("Authorization", "Bearer "+p.config.Auth.Token)
	case "api_key":
		name := p.config.Auth.HeaderName
		if name == "" {
			name = "X-API-Key"
		}
		httpReq.Header.Set(name, p.config.Auth.Token)
	case "private_url":
		// Auth is embedded in the URL
	case "none", "":
		// No auth
	case "oauth":
		if p.config.Auth.Token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.config.Auth.Token)
		}
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to %s: %w", p.config.URL, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote server returned status %d", httpResp.StatusCode)
	}

	// Capture session ID if provided
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.sessionID = sid
	}

	return parseHTTPResponse(httpResp)
}
