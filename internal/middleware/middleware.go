// Package middleware implements a bidirectional processing pipeline for MCP
// JSON-RPC messages flowing through the proxy. Each middleware can inspect and
// modify both requests (before the backend) and responses (before the client).
package middleware

import (
	"context"
	"encoding/json"
	"log"

	"github.com/JeremiahChurch/mcp-wrangler/internal/mcp"
	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
)

// Middleware processes MCP messages flowing through the proxy.
type Middleware interface {
	// Name returns the unique identifier for this middleware.
	Name() string

	// ProcessRequest is called before the request reaches the backend.
	// Return modified request, or error to block the request entirely.
	ProcessRequest(ctx context.Context, req *mcp.Request, meta *RequestMeta) (*mcp.Request, error)

	// ProcessResponse is called before the response reaches the client.
	// Return modified response, or error to inject an error response.
	ProcessResponse(ctx context.Context, req *mcp.Request, resp *mcp.Response, meta *RequestMeta) (*mcp.Response, error)
}

// RequestMeta carries context about the current request for middleware decisions.
type RequestMeta struct {
	UserID     string
	ServerID   string
	ServerName string
	Method     string // e.g. "tools/call", "tools/list"
	ToolName   string // for tools/call: which tool
	ClientIP   string
	RequestID  string // JSON-RPC id as string
}

// Pipeline holds an ordered list of middleware and executes them in sequence.
type Pipeline struct {
	middlewares []Middleware
}

// NewPipeline creates an empty pipeline.
func NewPipeline() *Pipeline {
	return &Pipeline{}
}

// Add appends a middleware to the pipeline.
func (p *Pipeline) Add(m Middleware) {
	p.middlewares = append(p.middlewares, m)
}

// Len returns the number of middleware in the pipeline.
func (p *Pipeline) Len() int {
	return len(p.middlewares)
}

// ProcessRequest runs all middleware on the request in order.
// If any middleware returns an error, processing stops and the error is returned.
func (p *Pipeline) ProcessRequest(ctx context.Context, req *mcp.Request, meta *RequestMeta) (*mcp.Request, error) {
	var err error
	for _, m := range p.middlewares {
		req, err = m.ProcessRequest(ctx, req, meta)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}

// ProcessResponse runs all middleware on the response in reverse order.
// If any middleware returns an error, processing stops and the error is returned.
func (p *Pipeline) ProcessResponse(ctx context.Context, req *mcp.Request, resp *mcp.Response, meta *RequestMeta) (*mcp.Response, error) {
	var err error
	for i := len(p.middlewares) - 1; i >= 0; i-- {
		resp, err = p.middlewares[i].ProcessResponse(ctx, req, resp, meta)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// Registry holds middleware factories and builds pipelines from DB configs.
type Registry struct {
	factories          map[string]Factory
	store              *store.MiddlewareStore
	archiveDispatcher  *ArchiveDispatcher
}

// Factory creates a middleware instance from a JSON config.
type Factory func(config json.RawMessage, eventLogger EventLogger) (Middleware, error)

// EventLogger is a callback for middleware to log events.
type EventLogger func(evt *store.MiddlewareEvent)

// NewRegistry creates a registry with the built-in middleware factories.
func NewRegistry(mwStore *store.MiddlewareStore, archiveDispatcher *ArchiveDispatcher) *Registry {
	r := &Registry{
		factories:         make(map[string]Factory),
		store:             mwStore,
		archiveDispatcher: archiveDispatcher,
	}
	// Register built-in middleware
	r.Register("sanitizer", NewSanitizerFromConfig)
	r.Register("sizer", NewSizerFromConfig)
	r.Register("alerter", NewAlerterFromConfig)
	// Archive uses a closure to capture the shared dispatcher
	r.Register("archive", func(config json.RawMessage, logger EventLogger) (Middleware, error) {
		return NewArchiveFromConfig(config, logger, archiveDispatcher)
	})
	return r
}

// ArchiveDispatcher returns the shared archive dispatcher, or nil if not configured.
func (r *Registry) ArchiveDispatcher() *ArchiveDispatcher {
	return r.archiveDispatcher
}

// Register adds a middleware factory.
func (r *Registry) Register(name string, factory Factory) {
	r.factories[name] = factory
}

// BuildPipeline creates a pipeline for a specific server by loading configs from the DB.
func (r *Registry) BuildPipeline(serverID string) *Pipeline {
	configs, err := r.store.GetForServer(serverID)
	if err != nil {
		log.Printf("middleware: failed to load configs for server %s: %v", serverID, err)
		return NewPipeline()
	}

	pipeline := NewPipeline()
	for _, mc := range configs {
		if !mc.Enabled {
			continue
		}
		factory, ok := r.factories[mc.Middleware]
		if !ok {
			log.Printf("middleware: unknown middleware %q (server %s)", mc.Middleware, serverID)
			continue
		}

		logger := r.makeEventLogger(serverID)
		m, err := factory(mc.Config, logger)
		if err != nil {
			log.Printf("middleware: failed to create %q for server %s: %v", mc.Middleware, serverID, err)
			continue
		}
		pipeline.Add(m)
	}
	return pipeline
}

func (r *Registry) makeEventLogger(serverID string) EventLogger {
	return func(evt *store.MiddlewareEvent) {
		evt.ServerID = serverID
		if err := r.store.LogEvent(evt); err != nil {
			log.Printf("middleware: failed to log event: %v", err)
		}
	}
}
