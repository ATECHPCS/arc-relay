package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
)

// HTTPProxy forwards MCP requests to an HTTP-based MCP server.
type HTTPProxy struct {
	targetURL  string
	sessionID  string
	httpClient *http.Client
}

// NewHTTPProxy creates a proxy to an HTTP MCP server.
func NewHTTPProxy(targetURL string) *HTTPProxy {
	return &HTTPProxy{
		targetURL:  targetURL,
		httpClient: &http.Client{},
	}
}

// Send forwards an MCP request to the HTTP backend.
func (p *HTTPProxy) Send(ctx context.Context, req *mcp.Request) (*mcp.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if p.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", p.sessionID)
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to %s: %w", p.targetURL, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned status %d", httpResp.StatusCode)
	}

	// Capture session ID if provided
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.sessionID = sid
	}

	return parseHTTPResponse(httpResp)
}
