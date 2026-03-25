package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// ArchiveConfig configures the archive middleware.
type ArchiveConfig struct {
	URL          string `json:"url"`                      // Target URL to POST archived data
	AuthType     string `json:"auth_type"`                // "none", "bearer", "api_key"
	AuthValue    string `json:"auth_value"`               // Token/key value
	APIKeyHeader string `json:"api_key_header,omitempty"` // Header name for api_key auth (default: X-API-Key)
	Include      string `json:"include"`                  // "request", "response", "both"
}

// DefaultArchiveConfig returns sensible defaults.
func DefaultArchiveConfig() ArchiveConfig {
	return ArchiveConfig{
		URL:     "",
		Include: "both",
	}
}

// archivePayload is the JSON envelope sent to the archive target.
type archivePayload struct {
	Version   string          `json:"version"`
	Source    string          `json:"source"`
	Phase     string          `json:"phase"`
	Timestamp string          `json:"timestamp"`
	Meta      archiveMeta     `json:"meta"`
	Request   json.RawMessage `json:"request,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
}

type archiveMeta struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	UserID     string `json:"user_id"`
	ClientIP   string `json:"client_ip"`
	Method     string `json:"method"`
	ToolName   string `json:"tool_name"`
	RequestID  string `json:"request_id"`
}

// Archive sends MCP request/response data to a configured HTTP endpoint.
// It is observe-only and never blocks MCP traffic.
type Archive struct {
	cfg         ArchiveConfig
	eventLogger EventLogger
	httpClient  *http.Client
	sendCh      chan []byte
}

// NewArchiveFromConfig creates an Archive from JSON config.
func NewArchiveFromConfig(config json.RawMessage, logger EventLogger) (Middleware, error) {
	var cfg ArchiveConfig
	if len(config) > 0 && string(config) != "{}" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("archive: invalid config: %w", err)
		}
	} else {
		cfg = DefaultArchiveConfig()
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("archive: url is required")
	}
	if cfg.Include == "" {
		cfg.Include = "both"
	}
	if cfg.APIKeyHeader == "" {
		cfg.APIKeyHeader = "X-API-Key"
	}

	a := &Archive{
		cfg:         cfg,
		eventLogger: logger,
		httpClient:  &http.Client{Timeout: 2 * time.Second},
		sendCh:      make(chan []byte, 256),
	}
	go a.worker()
	return a, nil
}

func (a *Archive) Name() string { return "archive" }

func (a *Archive) ProcessRequest(ctx context.Context, req *mcp.Request, meta *RequestMeta) (*mcp.Request, error) {
	if a.cfg.Include != "request" {
		return req, nil // will archive in ProcessResponse with both request+response
	}

	reqJSON, _ := json.Marshal(req)
	payload := a.buildPayload("request", meta, reqJSON, nil)
	a.enqueue(payload)
	return req, nil
}

func (a *Archive) ProcessResponse(ctx context.Context, req *mcp.Request, resp *mcp.Response, meta *RequestMeta) (*mcp.Response, error) {
	if a.cfg.Include == "request" {
		return resp, nil // already archived in ProcessRequest
	}

	var reqJSON json.RawMessage
	var respJSON json.RawMessage

	if a.cfg.Include == "both" {
		reqJSON, _ = json.Marshal(req)
	}
	respJSON, _ = json.Marshal(resp)

	phase := "response"
	if a.cfg.Include == "both" {
		phase = "exchange"
	}

	payload := a.buildPayload(phase, meta, reqJSON, respJSON)
	a.enqueue(payload)
	return resp, nil
}

func (a *Archive) buildPayload(phase string, meta *RequestMeta, reqJSON, respJSON json.RawMessage) []byte {
	p := archivePayload{
		Version:   "v1",
		Source:    "mcp_wrangler",
		Phase:     phase,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Meta: archiveMeta{
			ServerID:   meta.ServerID,
			ServerName: meta.ServerName,
			UserID:     meta.UserID,
			ClientIP:   meta.ClientIP,
			Method:     meta.Method,
			ToolName:   meta.ToolName,
			RequestID:  meta.RequestID,
		},
		Request:  reqJSON,
		Response: respJSON,
	}
	body, _ := json.Marshal(p)
	return body
}

func (a *Archive) enqueue(body []byte) {
	select {
	case a.sendCh <- body:
	default:
		// Queue full - drop and log
		log.Printf("archive: queue full, dropping payload")
		if a.eventLogger != nil {
			a.eventLogger(&store.MiddlewareEvent{
				Middleware: "archive",
				EventType:  "dropped",
				Summary:    "archive queue full, payload dropped",
			})
		}
	}
}

func (a *Archive) worker() {
	for body := range a.sendCh {
		a.send(body)
	}
}

func (a *Archive) send(body []byte) {
	req, err := http.NewRequest("POST", a.cfg.URL, bytes.NewReader(body))
	if err != nil {
		log.Printf("archive: failed to create request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	switch a.cfg.AuthType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+a.cfg.AuthValue)
	case "api_key":
		req.Header.Set(a.cfg.APIKeyHeader, a.cfg.AuthValue)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		log.Printf("archive: POST to %s failed: %v", a.cfg.URL, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("archive: POST to %s returned status %d", a.cfg.URL, resp.StatusCode)
	}
}
