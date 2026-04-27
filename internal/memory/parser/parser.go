// Package parser converts AI-tool-specific JSONL transcripts into store.Message
// rows + compact events. v1 ships only the claude-code parser; codex and
// gemini follow as drop-in implementations registered via init().
package parser

import (
	"io"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// CompactEvent is one compaction record extracted from a transcript.
// Lives in this package (not in internal/memory) to keep the import graph acyclic
// once Task 4's service layer imports parser.
type CompactEvent struct {
	UUID             string
	SessionID        string
	Epoch            int
	Timestamp        string
	TriggerType      string
	TokenCountBefore int
}

// Parser converts a JSONL chunk into normalized store rows.
type Parser interface {
	Platform() string
	Parse(io.Reader) ([]*store.Message, []*CompactEvent, error)
}

// registry maps platform string → Parser. Populated by each parser's init().
var registry = map[string]Parser{}

// Register wires a parser into the registry. Called by init() in each
// per-platform file. Last registration wins for a given platform.
func Register(p Parser) {
	registry[p.Platform()] = p
}

// Get returns the parser for platform, or nil if no parser is registered.
// Service layer should treat nil as "unknown platform" → HTTP 400.
func Get(platform string) Parser {
	return registry[platform]
}

// Platforms returns the registered platform strings, useful for diagnostics
// (e.g. /api/memory/stats can surface "supported platforms").
func Platforms() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
