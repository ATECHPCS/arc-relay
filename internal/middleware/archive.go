package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/comma-compliance/arc-relay/internal/mcp"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// ArchiveConfig configures the archive middleware.
type ArchiveConfig struct {
	URL              string `json:"url"`                                 // Target URL to POST archived data
	AuthType         string `json:"auth_type"`                           // "none", "bearer", "api_key"
	AuthValue        string `json:"auth_value"`                          // Token/key value
	APIKeyHeader     string `json:"api_key_header,omitempty"`            // Header name for api_key auth (default: X-API-Key)
	Include          string `json:"include"`                             // "request", "response", "both"
	NaClRecipientKey string `json:"nacl_recipient_key,omitempty"`        // Base64-encoded Curve25519 public key for NaCl Box encryption
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

// Archive sends MCP request/response data to a configured HTTP endpoint
// via the shared ArchiveDispatcher. It is observe-only and never blocks MCP traffic.
type Archive struct {
	cfg         ArchiveConfig
	eventLogger EventLogger
	dispatcher  *ArchiveDispatcher
}

// NewArchiveFromConfig creates an Archive from JSON config.
func NewArchiveFromConfig(config json.RawMessage, logger EventLogger, dispatcher *ArchiveDispatcher) (Middleware, error) {
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
	if dispatcher == nil {
		return nil, fmt.Errorf("archive: dispatcher not available")
	}
	// Validate NaCl recipient key at config time so a bad key is caught immediately
	if cfg.NaClRecipientKey != "" {
		if _, err := decodeRecipientKey(cfg.NaClRecipientKey); err != nil {
			return nil, fmt.Errorf("archive: invalid nacl_recipient_key: %w", err)
		}
	}

	return &Archive{
		cfg:         cfg,
		eventLogger: logger,
		dispatcher:  dispatcher,
	}, nil
}

func (a *Archive) Name() string { return "archive" }

func (a *Archive) ProcessRequest(ctx context.Context, req *mcp.Request, meta *RequestMeta) (*mcp.Request, error) {
	if a.cfg.Include != "request" {
		return req, nil // will archive in ProcessResponse with both request+response
	}

	reqJSON, _ := json.Marshal(req)
	payload := a.buildPayload("request", meta, reqJSON, nil)
	a.enqueue(payload, meta)
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
	a.enqueue(payload, meta)
	return resp, nil
}

func (a *Archive) buildPayload(phase string, meta *RequestMeta, reqJSON, respJSON json.RawMessage) []byte {
	p := archivePayload{
		Version:   "v1",
		Source:    "arc_relay",
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

func (a *Archive) enqueue(body []byte, meta *RequestMeta) {
	payload := body
	if a.cfg.NaClRecipientKey != "" {
		recipientKey, err := decodeRecipientKey(a.cfg.NaClRecipientKey)
		if err != nil {
			log.Printf("archive: invalid nacl_recipient_key: %v", err)
			if a.eventLogger != nil {
				a.eventLogger(&store.MiddlewareEvent{
					Middleware: "archive",
					EventType:  "error",
					Summary:    "archive payload dropped: invalid nacl_recipient_key - " + err.Error(),
				})
			}
			return
		}
		encrypted, err := encryptPayload(payload, recipientKey)
		if err != nil {
			log.Printf("archive: encryption failed: %v", err)
			if a.eventLogger != nil {
				a.eventLogger(&store.MiddlewareEvent{
					Middleware: "archive",
					EventType:  "error",
					Summary:    "archive payload dropped: encryption failed - " + err.Error(),
				})
			}
			return
		}
		payload = encrypted
	}
	if err := a.dispatcher.EnqueueWithServer(payload, a.cfg, meta.ServerID); err != nil {
		log.Printf("archive: failed to enqueue: %v", err)
		if a.eventLogger != nil {
			a.eventLogger(&store.MiddlewareEvent{
				Middleware: "archive",
				EventType:  "error",
				Summary:    "failed to enqueue archive payload: " + err.Error(),
			})
		}
	}
}
