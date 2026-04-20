package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/comma-compliance/arc-relay/internal/mcp"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// AlerterConfig configures the alerter middleware.
type AlerterConfig struct {
	Rules []AlertRule `json:"rules"`
}

// AlertRule defines a pattern match rule that triggers alerts.
type AlertRule struct {
	Name       string `json:"name"`
	Match      string `json:"match"`      // regex pattern
	MatchSize  int    `json:"match_size"` // alert if response size > this (bytes)
	Direction  string `json:"direction"`  // "request", "response", "both"
	Action     string `json:"action"`     // "log", "webhook"
	WebhookURL string `json:"webhook_url"`
}

// DefaultAlerterConfig returns sensible defaults.
func DefaultAlerterConfig() AlerterConfig {
	return AlerterConfig{
		Rules: []AlertRule{
			{
				Name:      "production_access",
				Match:     `(?i)(production|prod[_-]db|master[_-]password)`,
				Direction: "request",
				Action:    "log",
			},
			{
				Name:      "large_response",
				MatchSize: 100000,
				Direction: "response",
				Action:    "log",
			},
		},
	}
}

type compiledRule struct {
	name       string
	re         *regexp.Regexp // nil if this is a size-only rule
	matchSize  int
	direction  string
	action     string
	webhookURL string
}

// Alerter fires alerts when content patterns match or responses exceed size thresholds.
type Alerter struct {
	rules       []compiledRule
	eventLogger EventLogger
	httpClient  *http.Client
}

// NewAlerterFromConfig creates an Alerter from JSON config.
func NewAlerterFromConfig(config json.RawMessage, logger EventLogger) (Middleware, error) {
	var cfg AlerterConfig
	if len(config) > 0 && string(config) != "{}" {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, fmt.Errorf("alerter: invalid config: %w", err)
		}
	} else {
		cfg = DefaultAlerterConfig()
	}
	return NewAlerter(cfg, logger)
}

// NewAlerter creates an alerter middleware with pre-compiled rules.
func NewAlerter(cfg AlerterConfig, logger EventLogger) (*Alerter, error) {
	rules := make([]compiledRule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		cr := compiledRule{
			name:       r.Name,
			matchSize:  r.MatchSize,
			direction:  r.Direction,
			action:     r.Action,
			webhookURL: r.WebhookURL,
		}
		if cr.direction == "" {
			cr.direction = "both"
		}
		if cr.action == "" {
			cr.action = "log"
		}
		if r.Match != "" {
			re, err := regexp.Compile(r.Match)
			if err != nil {
				return nil, fmt.Errorf("alerter: invalid regex for rule %q: %w", r.Name, err)
			}
			cr.re = re
		}
		rules = append(rules, cr)
	}
	return &Alerter{
		rules:       rules,
		eventLogger: logger,
		httpClient:  &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (a *Alerter) Name() string { return "alerter" }

func (a *Alerter) ProcessRequest(ctx context.Context, req *mcp.Request, meta *RequestMeta) (*mcp.Request, error) {
	text := string(req.Params)
	for _, r := range a.rules {
		if r.direction != "request" && r.direction != "both" {
			continue
		}
		if r.re != nil && r.re.MatchString(text) {
			summary := fmt.Sprintf("Alert rule %q matched in request: %s %s", r.name, meta.Method, meta.ToolName)
			a.fireAlert(r, meta, summary)
		}
	}
	// Alerter never blocks — it's observe-only
	return req, nil
}

func (a *Alerter) ProcessResponse(ctx context.Context, req *mcp.Request, resp *mcp.Response, meta *RequestMeta) (*mcp.Response, error) {
	if resp.Error != nil || resp.Result == nil {
		return resp, nil
	}

	text := string(resp.Result)
	size := len(resp.Result)

	for _, r := range a.rules {
		if r.direction != "response" && r.direction != "both" {
			continue
		}
		matched := false
		if r.re != nil && r.re.MatchString(text) {
			matched = true
		}
		if r.matchSize > 0 && size > r.matchSize {
			matched = true
		}
		if matched {
			summary := fmt.Sprintf("Alert rule %q matched in response: %s %s (size=%d)",
				r.name, meta.Method, meta.ToolName, size)
			a.fireAlert(r, meta, summary)
		}
	}
	// Alerter never modifies responses
	return resp, nil
}

func (a *Alerter) fireAlert(rule compiledRule, meta *RequestMeta, summary string) {
	// Always log the event
	if a.eventLogger != nil {
		a.eventLogger(&store.MiddlewareEvent{
			Middleware:    "alerter",
			EventType:     "alert",
			Summary:       summary,
			RequestMethod: meta.Method,
			EndpointName:  meta.ToolName,
			UserID:        meta.UserID,
		})
	}

	// Fire webhook if configured
	if rule.action == "webhook" && rule.webhookURL != "" {
		go a.sendWebhook(rule.webhookURL, summary, meta)
	}
}

func (a *Alerter) sendWebhook(rawURL, summary string, meta *RequestMeta) {
	if err := validateWebhookURL(rawURL); err != nil {
		slog.Warn("alerter: webhook URL rejected", "url", rawURL, "error", err)
		return
	}

	payload := map[string]string{
		"text":       summary,
		"server":     meta.ServerName,
		"method":     meta.Method,
		"tool":       meta.ToolName,
		"user":       meta.UserID,
		"request_id": meta.RequestID,
	}
	body, _ := json.Marshal(payload)

	resp, err := a.httpClient.Post(rawURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("alerter: webhook failed", "url", rawURL, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		slog.Warn("alerter: webhook returned non-success status", "url", rawURL, "status", resp.StatusCode)
	}
}

// validateWebhookURL enforces https and blocks private/loopback/link-local
// destinations so a misconfigured or attacker-influenced rule cannot exfiltrate
// MCP request context to internal services or cloud metadata endpoints.
func validateWebhookURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL")
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("webhook URL must use https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("webhook URL must include a host")
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateAddr(ip) {
			return fmt.Errorf("private/loopback addresses are not allowed")
		}
		return nil
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("cannot resolve host: %w", err)
	}
	for _, ipStr := range ips {
		if ip := net.ParseIP(ipStr); ip != nil && isPrivateAddr(ip) {
			return fmt.Errorf("host resolves to private/loopback address")
		}
	}
	return nil
}

func isPrivateAddr(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
