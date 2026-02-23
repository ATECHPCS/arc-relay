package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
)

// HTTPProxy forwards MCP requests to an HTTP-based MCP server.
type HTTPProxy struct {
	targetURL  string
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

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to %s: %w", p.targetURL, err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned status %d: %s", httpResp.StatusCode, string(respBody))
	}

	var resp mcp.Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}
