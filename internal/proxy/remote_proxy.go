package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/oauth"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// RemoteProxy forwards MCP requests to a remote MCP server with auth.
type RemoteProxy struct {
	serverID     string
	config       store.RemoteConfig
	sessionID    string
	httpClient   *http.Client
	oauthManager *oauth.Manager
}

// NewRemoteProxy creates a proxy to a remote MCP server.
func NewRemoteProxy(serverID string, config store.RemoteConfig, oauthMgr *oauth.Manager) *RemoteProxy {
	return &RemoteProxy{
		serverID:     serverID,
		config:       config,
		httpClient:   &http.Client{},
		oauthManager: oauthMgr,
	}
}

// applyAuth sets the appropriate auth header on the request.
func (p *RemoteProxy) applyAuth(ctx context.Context, req *http.Request) error {
	switch p.config.Auth.Type {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+p.config.Auth.Token)
	case "api_key":
		name := p.config.Auth.HeaderName
		if name == "" {
			name = "X-API-Key"
		}
		req.Header.Set(name, p.config.Auth.Token)
	case "private_url", "none", "":
		// No header needed
	case "oauth":
		if p.oauthManager == nil {
			return fmt.Errorf("OAuth manager not configured")
		}
		token, err := p.oauthManager.GetAccessToken(ctx, p.serverID)
		if err != nil {
			return fmt.Errorf("getting OAuth token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// SendNotification sends a fire-and-forget notification to the remote server.
func (p *RemoteProxy) SendNotification(n *mcp.Notification) error {
	body, _ := json.Marshal(n)
	httpReq, err := http.NewRequest("POST", p.config.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", p.sessionID)
	}
	if err := p.applyAuth(context.Background(), httpReq); err != nil {
		return err
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Send forwards an MCP request to the remote server.
func (p *RemoteProxy) Send(ctx context.Context, req *mcp.Request) (*mcp.Response, error) {
	resp, statusCode, err := p.doSend(ctx, req)

	// If 401 and OAuth, try refreshing token and retry once
	if statusCode == http.StatusUnauthorized && p.config.Auth.Type == "oauth" && p.oauthManager != nil {
		log.Printf("OAuth 401 for server %s, attempting token refresh", p.serverID)
		if refreshErr := p.oauthManager.ForceRefresh(ctx, p.serverID); refreshErr != nil {
			return nil, fmt.Errorf("token refresh after 401 failed: %w", refreshErr)
		}
		resp, _, err = p.doSend(ctx, req)
	}

	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *RemoteProxy) doSend(ctx context.Context, req *mcp.Request) (*mcp.Response, int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.config.URL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("creating http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if p.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", p.sessionID)
	}

	if err := p.applyAuth(ctx, httpReq); err != nil {
		return nil, 0, fmt.Errorf("applying auth: %w", err)
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("sending request to %s: %w", p.config.URL, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusUnauthorized {
		return nil, http.StatusUnauthorized, fmt.Errorf("remote server returned status 401")
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, httpResp.StatusCode, fmt.Errorf("remote server returned status %d", httpResp.StatusCode)
	}

	// Capture session ID if provided
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.sessionID = sid
	}

	resp, err := parseHTTPResponse(httpResp)
	return resp, http.StatusOK, err
}
