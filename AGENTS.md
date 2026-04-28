# Arc Relay - AI Contributor Guide

This file provides context for AI coding tools (Claude Code, Cursor, Copilot) working on Arc Relay.

## What is Arc Relay?

An open-source MCP (Model Context Protocol) control plane. Not just a proxy - it provides auth, user management, middleware-based policy controls, traffic interception, and archiving for AI tool calls. Built in Go with SQLite storage and a server-rendered web UI.

## Project Structure

```
cmd/
  arc-relay/          Server binary (CGO required - SQLite)
  arc-sync/           CLI tool (pure Go, no CGO)
internal/
  config/             TOML config parsing + env var overrides
  docker/             Docker container lifecycle management
  mcp/                MCP protocol types and endpoint classification
  middleware/          Bidirectional middleware pipeline
    archive.go         Archive middleware (observe-only, sends to webhook)
    archive_dispatcher.go  Durable delivery with retry + circuit breaker
    archive_encrypt.go     NaCl Box payload encryption
    sanitizer.go       Pattern-based PII redaction/blocking
    sizer.go           Response size enforcement
    alerter.go         Pattern + size alerting (log/webhook)
    middleware.go      Pipeline core, registry, factory pattern
  proxy/               MCP proxy backends
    proxy.go           Server manager, lifecycle, enumeration
    stdio_bridge.go    Docker stdin/stdout bridge
    http_proxy.go      HTTP forward proxy
    remote_proxy.go    Remote servers with OAuth
    sse.go             SSE response parsing
    health.go          Health monitoring + auto-recovery
  memory/              Transcript ingestion + recall service (separate DB)
    service.go         Ingest, three-tier FTS5 escalation, session extract
    parser/            Per-platform JSONL parsers (claudecode v1)
  mcp/memory/          Native MCP server exposing /mcp/memory (8 tools)
  skills/              Skill repository service (validation, archive disk I/O)
  store/               SQLite persistence
    db.go              Connection, migrations, backups
    users.go           Users, passwords (bcrypt), API keys (SHA-256)
    sessions.go        Web sessions
    crypto.go          AES-256-GCM config encryption
    middleware.go      Middleware config + events
    archive_queue.go   Durable delivery queue
    access.go          Endpoint access tiers
    request_logs.go    Audit logging
    memory_messages.go Memory FTS5 store (separate memory.db)
    memory_sessions.go Memory session metadata (separate memory.db)
    skills.go          Skills + skill_versions + skill_assignments CRUD
  web/                 HTTP handlers + templates
    handlers.go        All route handlers
    memory_handlers.go REST handlers for /api/memory/*
    memory_dashboard.go Web UI handlers for /memory pages
    skills_handlers.go REST handlers for /api/skills/*
    skills_dashboard.go Web UI handlers for /skills pages
    templates/         Server-rendered HTML (16+ templates)
    oauth_provider.go  OAuth 2.1 authorization server
    device_auth.go     Device code flow for CLI auth
  cli/                 CLI shared packages
    config/            arc-sync config (~/.config/arc-sync/)
    sync/              .mcp.json sync, memory watcher, skill install/sync
    relay/             HTTP client for Arc Relay API (servers, skills, memory)
    project/           Project detection (Claude Code, Cursor)
    safety/            Git safety checks
  oauth/               OAuth 2.1 client (PKCE, auto-discovery)
  auth/                Auth utilities
  catalog/             MCP server registry
migrations/            Embedded SQL migrations for arc-relay.db (001-015)
migrations-memory/     Embedded SQL migrations for memory.db (separate from above)
skills/arc-sync/       Claude Code skill definition (also //go:embed'd in arc-sync)
```

## Building and Testing

```bash
# Server (requires gcc, libsqlite3-dev). The sqlite_fts5 build tag is REQUIRED
# for memory recall — the bundled mattn/go-sqlite3 build does not include FTS5
# by default and the memory.db migrations will fail without it.
CGO_ENABLED=1 go build -tags sqlite_fts5 ./cmd/arc-relay

# CLI (pure Go, cross-platform; no FTS5 needed since the CLI never opens the DB)
CGO_ENABLED=0 go build ./cmd/arc-sync

# Tests
go test -tags sqlite_fts5 ./...          # Full suite (use the tag — memory tests fail without it)
go test -tags sqlite_fts5 -race ./...    # With race detector
go vet ./...                             # Static analysis

# Quick dev cycle
make build-all         # Both binaries
make run               # Build + run with example config
```

## Key Abstractions

### Middleware Pipeline

The core value proposition. Middleware processes MCP traffic bidirectionally:

```go
type Middleware interface {
    Name() string
    ProcessRequest(ctx context.Context, req *mcp.Request, meta *RequestMeta) (*mcp.Request, error)
    ProcessResponse(ctx context.Context, req *mcp.Request, resp *mcp.Response, meta *RequestMeta) (*mcp.Response, error)
}
```

Request middleware runs in priority order. Response middleware runs in reverse. Middleware can modify, block, or observe traffic.

**To add a new middleware:**
1. Create `internal/middleware/your_middleware.go`
2. Implement the `Middleware` interface
3. Add a factory function: `NewYourMiddlewareFromConfig(config json.RawMessage, logger EventLogger, ...) (Middleware, error)`
4. Register the factory in `middleware.go` `NewRegistry()` function
5. Add tests in `your_middleware_test.go`

### Proxy Backends

Three transport types for MCP servers:

- **Stdio** - Docker containers with stdin/stdout bridge (StdioBridge)
- **HTTP** - Direct HTTP POST to MCP endpoint (HTTPProxy)
- **Remote** - External servers with optional OAuth (RemoteProxy)

### Store Layer

SQLite with WAL mode, foreign keys, embedded migrations. The `ConfigEncryptor` optionally encrypts sensitive fields at rest using AES-256-GCM.

The relay opens **two SQLite files**:
- `arc-relay.db` — servers, users, OAuth state, middleware configs, skills metadata. Migrations under `migrations/`.
- `memory.db` — transcript sessions + messages + FTS5 index. Migrations under `migrations-memory/`. Isolated WAL/VACUUM/backup so heavy ingest does not contend with auth-critical writes.

### Memory Subsystem

`internal/memory/service.go` orchestrates ingest (parser-routed JSONL → bulk insert) and three-tier search escalation (raw FTS5 → quoted phrase → Go regex). Per-user scoping at every read surface; user ID always comes from the authenticated context, never the request body. Recall surfaces all prepend a `## RESEARCH ONLY ...` banner so an LLM consumer cannot mistake recalled history for live instructions.

### Skill Repository

`internal/skills/service.go` validates uploaded archives (gzipped tar, SKILL.md at root, YAML frontmatter, name==slug match, traversal rejection, 5 MiB cap, semver pins, SHA-256 integrity) before atomic disk write. The `.arc-sync-version` JSON marker (slug + version + sha256 + relay_url) inside each managed install distinguishes arc-sync-managed dirs from hand-installed ones — sync/remove refuse to touch unmarkered dirs. `arc-sync` duplicates the wire shapes in `internal/cli/relay/skills.go` to stay CGO-free.

### Auth

- **Web UI:** Session cookies (bcrypt passwords, session table in SQLite)
- **API/Proxy:** Bearer API keys (SHA-256 hashed, never stored plaintext)
- **OAuth 2.1:** PKCE flows for remote MCP servers, device code flow for CLI

## Configuration

TOML config file with environment variable overrides:

| Env Var | Config Key | Default |
|---------|-----------|---------|
| `ARC_RELAY_ENCRYPTION_KEY` | `encryption.key` | (required) |
| `ARC_RELAY_SESSION_SECRET` | `auth.session_secret` | (required) |
| `ARC_RELAY_ADMIN_PASSWORD` | `auth.admin_password` | (random) |
| `ARC_RELAY_DB_PATH` | `database.path` | `arc-relay.db` |
| `ARC_RELAY_MEMORY_DB_PATH` | `database.memory_path` | `<db_dir>/memory.db` |
| `ARC_RELAY_SKILLS_DIR` | `skills.bundles_dir` | `<db_dir>/skills` |
| `ARC_RELAY_BASE_URL` | `server.base_url` | `http://localhost:PORT` |
| `ARC_RELAY_PORT` | `server.port` | `8080` |
| `ARC_RELAY_LLM_API_KEY` | `llm.api_key` | (optional, optimizer disabled) |
| `ARC_RELAY_LLM_MODEL` | `llm.model` | `claude-haiku-4-5-20251001` |
| `ARC_RELAY_SENTRY_DSN` | `sentry_dsn` | (disabled) |

## Code Style

- Standard Go conventions (gofmt, go vet)
- Table-driven tests
- No external test frameworks - stdlib `testing` only
- Errors wrap with `fmt.Errorf("context: %w", err)`
- Middleware never blocks MCP traffic unless explicitly configured to (archive is observe-only)
