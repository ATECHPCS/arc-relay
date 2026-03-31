# MCP Wrangler - Technical Specification

## Overview

MCP Wrangler is a lightweight management system for deploying, proxying, and sharing MCP (Model Context Protocol) servers. It consolidates multiple MCP servers behind a single gateway with simple authentication and RBAC, making it easy to expose MCP capabilities to AI tools like Claude Desktop, Claude Code, and others.

**Goals:** Simpler alternative to [microsoft/mcp-gateway](https://github.com/microsoft/mcp-gateway) — no Kubernetes, no Azure dependencies, no .NET. Just a single Go binary + Docker.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      MCP Wrangler                           │
│                                                             │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────────┐  │
│  │ Web UI   │  │ Admin API    │  │ MCP Proxy Layer       │  │
│  │ (HTML    │  │ (REST)       │  │                       │  │
│  │ templates)│  │              │  │ /mcp/pfsense-prod ──────►│ Docker: pfsense-mcp (stdio)
│  │          │  │              │  │ /mcp/pfsense-dev  ──────►│ Docker: pfsense-mcp (stdio)
│  │          │  │              │  │ /mcp/uptime-kuma  ──────►│ Docker: uptime-kuma-mcp (HTTP)
│  │          │  │              │  │ /mcp/homeassistant ─────►│ Remote: HA MCP server
│  │          │  │              │  │ /mcp/sentry ────────────►│ Remote: mcp.sentry.dev (OAuth)
│  └──────────┘  └──────────────┘  └───────────────────────┘  │
│                                                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐   │
│  │ Auth Layer   │  │ Server Mgr   │  │ Docker Client    │   │
│  │ (API keys,   │  │ (lifecycle,  │  │ (container mgmt) │   │
│  │  sessions)   │  │  health)     │  │                  │   │
│  └──────────────┘  └──────────────┘  └──────────────────┘   │
│                                                             │
│  ┌──────────────────────────────────────────────────────┐   │
│  │ SQLite Database                                      │   │
│  │ (servers, users, API keys, RBAC, logs)               │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

### Key Architectural Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Language | **Go** | Single binary, excellent networking/proxy, good subprocess mgmt, low memory |
| Stdio server mgmt | **Docker containers** | Isolation, reproducibility, health checks, no dependency conflicts |
| Frontend | **Server-rendered HTML** (Go templates) | No build step, no JS framework, fast to build |
| Routing | **Path-based** on single port | Simpler networking, easy reverse proxy, single TLS cert |
| Database | **SQLite** | Zero-config, embedded, sufficient for this scale |
| Config | **TOML** for app config, DB for server/user state | TOML is readable, DB for dynamic state |

---

## MCP Server Types

MCP Wrangler supports three server types, each with different lifecycle management:

### 1. Stdio (Docker-wrapped)

The server runs as a subprocess inside a Docker container managed by MCP Wrangler. MCP Wrangler communicates with it over stdin/stdout via `docker exec` or by running a bridge process inside the container.

**Lifecycle:** MCP Wrangler builds/pulls the image, starts the container, and manages the stdio bridge. The bridge translates between Streamable HTTP (exposed to clients) and stdio (to the server process).

**Examples:** pfSense MCP Server (Python, stdio)

**Config inputs:**
- Docker image (or Dockerfile/repo URL to build from)
- Environment variables (key-value pairs, stored encrypted)
- Optional: custom command, working directory

### 2. HTTP (Docker or external)

The server exposes an HTTP endpoint (Streamable HTTP or legacy SSE). It may run in a Docker container managed by MCP Wrangler, or be an external service.

**Lifecycle:** For Docker-managed, MCP Wrangler starts the container and proxies to its HTTP port. For external, MCP Wrangler just proxies.

**Examples:** Uptime Kuma MCP Server (Python, SSE/HTTP on port 8000)

**Config inputs:**
- Docker image + port mapping, OR external URL
- Environment variables
- Health check endpoint (optional)

### 3. Remote

The server is hosted externally. MCP Wrangler acts as a pure proxy, forwarding MCP protocol messages.

**Lifecycle:** No lifecycle management. MCP Wrangler stores connection details and credentials, proxies requests.

**Examples:**
- Home Assistant MCP (HA add-on, private URL with embedded auth token)
- Sentry MCP (OAuth flow via `mcp.sentry.dev`)

**Config inputs:**
- Remote URL (may contain embedded auth, as with HA's private URL scheme)
- Auth type: none, private URL (auth in URL), bearer token, API key header, or OAuth
- OAuth config: client ID, auth URL, token URL, scopes (for OAuth servers)

---

## Core Features

### F1: Add/Manage MCP Server

**Web UI flow:**
1. User clicks "Add Server"
2. Selects type: Stdio (Docker), HTTP (Docker), HTTP (External), Remote
3. Fills in config form:
   - Name (slug, used in URL path)
   - Display name
   - Type-specific fields (image, env vars, URL, auth)
4. System validates config, pulls/builds image if needed
5. System starts server (if managed) and runs MCP `initialize` to verify connectivity
6. Server appears in dashboard

**API:**
```
POST   /api/servers           - Create server
GET    /api/servers           - List servers
GET    /api/servers/:id       - Get server detail
PUT    /api/servers/:id       - Update server
DELETE /api/servers/:id       - Delete server
POST   /api/servers/:id/start - Start managed server
POST   /api/servers/:id/stop  - Stop managed server
```

**Server record (DB schema):**
```sql
CREATE TABLE servers (
    id          TEXT PRIMARY KEY,  -- UUID
    name        TEXT UNIQUE NOT NULL,  -- slug for URL path
    display_name TEXT NOT NULL,
    server_type TEXT NOT NULL,  -- 'stdio', 'http', 'remote'
    config      TEXT NOT NULL,  -- JSON: image, env vars, url, auth, etc.
    status      TEXT NOT NULL DEFAULT 'stopped',  -- stopped, starting, running, error
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

### F2: List Servers & Enumerate Endpoints

Once a server is running and connected, MCP Wrangler calls `tools/list`, `resources/list`, and `prompts/list` on the server and caches the results.

**Web UI:** Dashboard shows all servers with status, and expandable sections showing their tools, resources, and prompts.

**RBAC per endpoint:**
- Each tool/resource/prompt can be allowed or denied per user/role
- Default: all endpoints allowed for all users
- Admin can toggle individual endpoint access per user

```sql
CREATE TABLE endpoint_permissions (
    id          TEXT PRIMARY KEY,
    server_id   TEXT NOT NULL REFERENCES servers(id),
    endpoint_type TEXT NOT NULL,  -- 'tool', 'resource', 'prompt'
    endpoint_name TEXT NOT NULL,
    user_id     TEXT REFERENCES users(id),  -- NULL = default for all users
    allowed     BOOLEAN NOT NULL DEFAULT TRUE,
    UNIQUE(server_id, endpoint_type, endpoint_name, user_id)
);
```

### F3: Proxy MCP Servers

Each server is exposed at `/mcp/{server-name}`. MCP Wrangler implements Streamable HTTP transport (the current MCP standard) on the client-facing side, regardless of the backend server's transport.

**Proxy flow:**
```
AI Client (Claude, etc.)
    │
    │  Streamable HTTP (POST/GET with SSE)
    │  + Auth header (Bearer token)
    ▼
MCP Wrangler (/mcp/{server-name})
    │
    ├─► Auth check (validate token, check user permissions)
    ├─► RBAC filter (strip disallowed tools/resources from responses)
    │
    ├─► [stdio server] ──► Docker container stdin/stdout
    ├─► [http server]  ──► HTTP proxy to container or external URL
    └─► [remote server] ─► HTTP proxy to remote URL (with stored credentials)
```

**Session management:** MCP Wrangler manages sessions per client connection. For stdio backends, it maintains the subprocess session. For HTTP/remote backends, it forwards session IDs.

**Key proxy behaviors:**
- `initialize` requests: forwarded, response cached for endpoint enumeration
- `tools/list`: response filtered by RBAC before returning to client
- `tools/call`: checked against RBAC, forwarded if allowed, error if denied
- Same pattern for `resources/*` and `prompts/*`

### F4: User Management

Simple user system for the POC:

```sql
CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    username    TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,  -- bcrypt
    role        TEXT NOT NULL DEFAULT 'user',  -- 'admin' or 'user'
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE api_keys (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id),
    key_hash    TEXT NOT NULL,  -- SHA-256 hash of the key
    name        TEXT NOT NULL,  -- human label
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_used   DATETIME,
    revoked     BOOLEAN DEFAULT FALSE
);
```

**Auth methods:**
- **Web UI:** Session cookie (login with username/password)
- **MCP proxy endpoints:** Bearer token (API key from `api_keys` table)

**Web UI pages:**
- User list (admin only)
- Create/edit user
- API key management (generate, revoke, list)

### F5: Logging (stretch)

```sql
CREATE TABLE request_logs (
    id          TEXT PRIMARY KEY,
    timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP,
    user_id     TEXT REFERENCES users(id),
    server_id   TEXT REFERENCES servers(id),
    method      TEXT NOT NULL,  -- MCP method: tools/call, resources/read, etc.
    endpoint_name TEXT,  -- specific tool/resource name
    duration_ms INTEGER,
    status      TEXT,  -- success, error
    error_msg   TEXT
);
```

All MCP requests proxied through MCP Wrangler are logged. For stdio servers, stderr output is captured and stored separately.

### F6: Connection Config Generation (stretch)

Generate ready-to-paste config snippets for connecting to MCP Wrangler servers:

**Claude Desktop (`claude_desktop_config.json`):**
```json
{
  "mcpServers": {
    "pfsense-prod": {
      "url": "http://mcp-wrangler.local:8080/mcp/pfsense-prod",
      "headers": {
        "Authorization": "Bearer <your-api-key>"
      }
    }
  }
}
```

**Claude Code:**
```bash
claude mcp add --transport http pfsense-prod http://mcp-wrangler.local:8080/mcp/pfsense-prod --header "Authorization: Bearer <your-api-key>"
```

### F7: Access Logs & Analytics (stretch)

Web UI dashboards showing:
- Request counts per server, per endpoint, per user
- Error rates
- Response time percentiles
- Timeline charts (basic, server-rendered with a lightweight chart library)

---

## Stdio-to-HTTP Bridge Design

This is the most complex piece. MCP Wrangler needs to translate between Streamable HTTP (what clients connect with) and stdio (what the server subprocess speaks).

### Approach: Bridge Process per Connection

```
Client ──HTTP──► MCP Wrangler ──stdin/stdout──► Docker Container
                     │                              │
                     │  Manages session              │  Runs MCP server
                     │  Translates HTTP↔stdio        │  (e.g., pfsense-mcp)
                     │  Buffers JSON-RPC messages     │
```

**Implementation:**
1. Docker container runs the MCP server process (e.g., `python -m pfsense_mcp`)
2. MCP Wrangler attaches to the container's stdin/stdout via Docker API (`ContainerAttach`)
3. For each client session:
   - Client POSTs a JSON-RPC message to `/mcp/{server-name}`
   - MCP Wrangler writes the message + newline to the container's stdin
   - MCP Wrangler reads the response from stdout (newline-delimited JSON-RPC)
   - Response is returned to client as JSON or SSE stream

**Concurrency consideration:** Stdio is inherently single-session. Options:
- **One container per client session** — simplest, most isolated, but resource-heavy
- **Multiplexed access with request queuing** — single container, serialize requests, match responses by JSON-RPC id. Most efficient.
- **Recommended for POC:** Single container per server, multiplexed access with request queuing. Upgrade to per-session containers if needed.

---

## Example Server Configurations

### pfSense MCP (stdio, Docker)
```json
{
  "name": "pfsense-prod",
  "display_name": "pfSense - Production Firewall",
  "server_type": "stdio",
  "config": {
    "image": "ghcr.io/your-org/pfsense-mcp-server:latest",
    "command": ["python", "-m", "pfsense_mcp"],
    "env": {
      "PFSENSE_URL": "https://pfsense-prod.local",
      "PFSENSE_API_KEY": "encrypted:...",
      "PFSENSE_VERSION": "2.7.0",
      "AUTH_METHOD": "api_key",
      "VERIFY_SSL": "false"
    }
  }
}
```

### pfSense MCP - Dev (stdio, Docker)
```json
{
  "name": "pfsense-dev",
  "display_name": "pfSense - Dev Firewall",
  "server_type": "stdio",
  "config": {
    "image": "ghcr.io/your-org/pfsense-mcp-server:latest",
    "command": ["python", "-m", "pfsense_mcp"],
    "env": {
      "PFSENSE_URL": "https://pfsense-dev.local",
      "PFSENSE_API_KEY": "encrypted:...",
      "PFSENSE_VERSION": "2.7.0",
      "AUTH_METHOD": "api_key",
      "VERIFY_SSL": "false"
    }
  }
}
```

### Uptime Kuma (HTTP, Docker)
```json
{
  "name": "uptime-kuma",
  "display_name": "Uptime Kuma Monitoring",
  "server_type": "http",
  "config": {
    "image": "ghcr.io/camusama/uptime-kuma-mcp-server:latest",
    "port": 8000,
    "env": {
      "KUMA_URL": "http://uptime-kuma.local:3001",
      "KUMA_USERNAME": "admin",
      "KUMA_PASSWORD": "encrypted:..."
    },
    "health_check": "/health"
  }
}
```

### Home Assistant (Remote, private URL auth)
```json
{
  "name": "homeassistant",
  "display_name": "Home Assistant",
  "server_type": "remote",
  "config": {
    "url": "http://homeassistant.local:8123/api/mcp/xxxxxxxxxxxxxxxx",
    "auth": {
      "type": "private_url"
    }
  }
}
```
> **Note:** HA MCP runs as a Home Assistant add-on and exposes a Streamable HTTP
> endpoint with a private URL containing an embedded auth token (the path segment).
> Auth is baked into the URL itself — no separate token header needed.

### Sentry (Remote, OAuth)
```json
{
  "name": "sentry",
  "display_name": "Sentry Error Tracking",
  "server_type": "remote",
  "config": {
    "url": "https://mcp.sentry.dev/mcp",
    "auth": {
      "type": "oauth",
      "auth_url": "https://sentry.io/oauth/authorize/",
      "token_url": "https://sentry.io/oauth/token/",
      "client_id": "...",
      "client_secret": "encrypted:...",
      "scopes": ["org:read", "project:read", "event:read"]
    }
  }
}
```

---

## Project Structure

```
mcp-wrangler/
├── cmd/
│   └── mcp-wrangler/
│       └── main.go              # Entry point
├── internal/
│   ├── config/
│   │   └── config.go            # TOML config loading
│   ├── server/
│   │   ├── http.go              # HTTP server setup, routing
│   │   └── middleware.go        # Auth middleware
│   ├── proxy/
│   │   ├── proxy.go             # MCP proxy handler (dispatches by type)
│   │   ├── stdio_bridge.go      # Stdio↔HTTP bridge
│   │   ├── http_proxy.go        # HTTP/SSE reverse proxy
│   │   └── remote_proxy.go      # Remote server proxy (incl. OAuth)
│   ├── docker/
│   │   └── manager.go           # Docker container lifecycle
│   ├── mcp/
│   │   ├── protocol.go          # MCP JSON-RPC types
│   │   ├── session.go           # Session management
│   │   └── enumerate.go         # Tool/resource/prompt enumeration
│   ├── auth/
│   │   ├── auth.go              # Auth service (login, API keys)
│   │   └── rbac.go              # RBAC checking
│   ├── store/
│   │   ├── db.go                # SQLite setup, migrations
│   │   ├── servers.go           # Server CRUD
│   │   ├── users.go             # User CRUD
│   │   └── logs.go              # Request logging
│   └── web/
│       ├── handlers.go          # Web UI handlers
│       └── templates/           # Go HTML templates
│           ├── layout.html
│           ├── dashboard.html
│           ├── server_form.html
│           ├── server_detail.html
│           ├── users.html
│           └── api_keys.html
├── migrations/
│   └── 001_initial.sql
├── config.example.toml
├── Dockerfile
├── docker-compose.yml
└── go.mod
```

---

## App Config (TOML)

```toml
[server]
host = "0.0.0.0"
port = 8080

[database]
path = "/data/mcp-wrangler.db"

[docker]
# Docker socket path (default for Linux)
socket = "unix:///var/run/docker.sock"
# Network for managed containers
network = "mcp-wrangler"

[encryption]
# Key for encrypting stored credentials (generate with: openssl rand -hex 32)
key = "your-encryption-key-here"

[auth]
# Session secret for web UI cookies
session_secret = "your-session-secret-here"
# Default admin password (only used on first run)
admin_password = "changeme"
```

---

## Docker Deployment

```yaml
# docker-compose.yml
version: "3.8"
services:
  mcp-wrangler:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock  # Docker-in-Docker for managed servers
      - wrangler-data:/data
    environment:
      - MCP_WRANGLER_ENCRYPTION_KEY=changeme
      - MCP_WRANGLER_SESSION_SECRET=changeme
      - MCP_WRANGLER_ADMIN_PASSWORD=changeme

volumes:
  wrangler-data:
```

---

## Implementation Phases

### Phase 1: Foundation (MVP)
1. Project scaffolding, Go module, config loading
2. SQLite database with migrations
3. Docker client integration (pull, start, stop, attach containers)
4. Stdio bridge: translate Streamable HTTP ↔ stdio via Docker attach
5. Basic MCP proxy: forward `initialize`, `tools/list`, `tools/call`
6. Wire up one pfSense server end-to-end through the proxy
7. Simple API key auth on proxy endpoints

### Phase 2: Server Management
8. REST API for server CRUD
9. Web UI: dashboard, add/edit server forms
10. Support HTTP server type (Docker-managed): proxy to container's HTTP port
11. Support remote server type: proxy with bearer token auth
12. Server health monitoring (periodic `ping` or health check)

### Phase 3: Multi-server & Auth
13. Multiple servers running simultaneously
14. User management (create, edit, delete users)
15. API key management (generate, revoke, list per user)
16. Web UI login with session cookies
17. RBAC: per-endpoint permissions per user
18. MCP endpoint enumeration (cache tools/resources/prompts per server)

### Phase 4: Remote & OAuth
19. OAuth flow for remote servers (Sentry-style)
20. Token storage and refresh
21. All 5 example servers working end-to-end

### Phase 5: Polish & Fixes
22. Request logging to DB
23. Connection config generation (Claude Desktop, Claude Code snippets)
24. Access logs dashboard with basic charts
25. Consolidated stderr/log capture for managed servers
26. Fix connection examples to use base URL scheme (https:// when configured)

### Phase 6: Auto-Build Stdio Images from Packages

Most MCP servers are distributed as npm or pip packages, not Docker images. Users shouldn't need to find or build Docker images manually. MCP Wrangler should auto-generate and build Docker images from package metadata.

#### Problem

Stdio MCP servers are distributed as:
- **npm packages** (`npx @anthropic/mcp-server-github`) — most common
- **pip packages** (`uvx mcp-server-fetch`, `pip install pfsense-mcp-server`) — second most
- **Git repos with Dockerfile** — project-specific (e.g., pfsense-mcp-server fork)
- **Git repos without Dockerfile** — smaller community projects
- **Pre-built Docker images** — rare, only larger projects

Currently users must supply a Docker image, which means they either find a rare pre-built one, build it themselves, or give up.

#### Solution: Package-to-Docker Builder

Add a new input mode for stdio servers: **"Build from Package"** alongside the existing "Docker Image" field.

**Form fields:**

| Field | Required | Example |
|-------|----------|---------|
| Runtime | Yes | `python` / `node` (dropdown) |
| Package | Yes | `pfsense-mcp-server` or `@anthropic/mcp-server-github` |
| Version | No (default: latest) | `1.2.3` |
| Git URL | No (alternative to package) | `https://github.com/user/repo` |
| Custom Dockerfile | No (escape hatch) | Full Dockerfile text |

**Dockerfile templates:**

```dockerfile
# Python (pip)
FROM python:3.11-slim
RUN pip install --no-cache-dir {{package}}{{if version}}=={{version}}{{end}}
ENTRYPOINT ["{{entry_command}}"]
# entry_command auto-detected or defaulted to: python -m {{module_name}}
```

```dockerfile
# Node (npm)
FROM node:20-slim
RUN npm install -g {{package}}{{if version}}@{{version}}{{end}}
ENTRYPOINT ["npx", "{{package}}"]
```

```dockerfile
# Git repo (clone + detect)
FROM python:3.11-slim  # or node:20-slim, detected from repo
RUN git clone {{git_url}} /app
WORKDIR /app
RUN pip install -r requirements.txt  # or npm install, detected
ENTRYPOINT ["python", "-m", "{{module}}"]
```

**Build + cache flow:**

1. User submits package info via form
2. Wrangler generates Dockerfile from template (or uses custom one)
3. Wrangler calls Docker build API: `docker build -t mcp-pkg/{{package}}:{{version}} -`
4. Image is cached locally — only rebuilds when version changes or user forces rebuild
5. Container starts from the built image using normal stdio flow

**Catalog integration:**

The catalog already knows package type and name. When `catalogSelectStdio()` fires for a server without a `docker_image`, auto-populate the runtime + package fields instead of leaving the user with an empty Docker Image field.

**DB schema change:**

Extend `StdioConfig` with optional build metadata:

```go
type StdioConfig struct {
    Image      string            // existing: pre-built image reference
    Build      *StdioBuildConfig  // new: auto-build from package
    Command    []string
    Entrypoint []string
    Env        map[string]string
}

type StdioBuildConfig struct {
    Runtime    string  // "python" or "node"
    Package    string  // pip/npm package name
    Version    string  // package version (empty = latest)
    GitURL     string  // alternative: build from git repo
    Dockerfile string  // alternative: custom Dockerfile text
}
```

When `Build` is set and `Image` is empty, Wrangler builds the image before starting the container. The built image tag is stored back in `Image` for cache.

**Rebuild triggers:**
- Version change in the build config
- User clicks "Rebuild Image" button on server detail page
- Force rebuild on server start (optional checkbox)

**Open questions:**
- Should we support private git repos? (Needs SSH key or token management)
- Should we support Rust/Go MCP servers? (Less common, but `cargo install` / `go install` patterns exist)
- Should auto-build run in a build container for isolation, or directly via the Docker daemon?
- Image cleanup: auto-prune old versions? Configurable retention?

### Phase 7: Proxy Middleware — Traffic Interception & Processing

MCP Wrangler sits at the chokepoint between AI clients and MCP servers. This position enables powerful traffic processing: sanitization, compliance enforcement, context optimization, and observability — without modifying any server or client.

#### Architecture

```
AI Client
    │
    │  JSON-RPC request
    ▼
┌─────────────────────────────────────────────────┐
│  MCP Wrangler Proxy                             │
│                                                 │
│  ┌─────────────────────────────────────────┐    │
│  │         Middleware Pipeline              │    │
│  │                                         │    │
│  │  Request ──► [Auth] ──► [RBAC]          │    │
│  │              ──► [Middleware 1]          │    │
│  │              ──► [Middleware 2]          │    │
│  │              ──► [Middleware N]          │    │
│  │              ──► Backend Server          │    │
│  │                                         │    │
│  │  Response ◄── [Middleware N]             │    │
│  │               ◄── [Middleware 2]         │    │
│  │               ◄── [Middleware 1]         │    │
│  │               ◄── Client                │    │
│  └─────────────────────────────────────────┘    │
└─────────────────────────────────────────────────┘
```

Middleware runs as a bidirectional pipeline: each middleware sees the request going in and the response coming back, and can modify, block, or annotate either.

#### Middleware Interface

```go
// Middleware processes MCP messages flowing through the proxy.
// Each middleware can inspect/modify both requests and responses.
type Middleware interface {
    // Name returns a unique identifier for this middleware.
    Name() string

    // ProcessRequest is called before the request reaches the backend.
    // Return modified request, or error to block the request.
    ProcessRequest(ctx context.Context, req *MCPMessage, meta *RequestMeta) (*MCPMessage, error)

    // ProcessResponse is called before the response reaches the client.
    // Return modified response, or error to inject an error response.
    ProcessResponse(ctx context.Context, resp *MCPMessage, meta *RequestMeta) (*MCPMessage, error)
}

// RequestMeta carries context about the request for middleware decisions.
type RequestMeta struct {
    UserID     string
    ServerID   string
    ServerName string
    Method     string      // e.g. "tools/call", "tools/list"
    ToolName   string      // for tools/call: which tool
    ClientIP   string
    RequestID  string
}

// MCPMessage is the parsed JSON-RPC message (request or response).
type MCPMessage struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      any             `json:"id,omitempty"`
    Method  string          `json:"method,omitempty"`
    Params  json.RawMessage `json:"params,omitempty"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *JSONRPCError   `json:"error,omitempty"`
}
```

#### Built-in Middleware (Day 1)

**1. Sanitizer — PII/Secret Redaction**

Scans tool call results for patterns that look like secrets, credentials, PII, and redacts them before they reach the AI client. Configurable patterns.

```yaml
sanitizer:
  enabled: true
  patterns:
    - name: api_key
      regex: '(?i)(api[_-]?key|secret|token|password)\s*[=:]\s*\S+'
      action: redact  # replace match with [REDACTED]
    - name: ssn
      regex: '\b\d{3}-\d{2}-\d{4}\b'
      action: redact
    - name: credit_card
      regex: '\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b'
      action: block   # block the entire response, return error
  custom_patterns: []  # user-defined via web UI
```

**2. Content Sizer — Context Window Optimization**

Measures and optionally truncates/summarizes large tool responses to prevent context window exhaustion. Reports size metrics for analytics.

```yaml
content_sizer:
  enabled: true
  max_response_tokens: 50000     # warn or truncate above this
  action: truncate_with_summary  # truncate | warn | summarize | pass
  summary_prompt: "Summarize this tool output, preserving key data..."
  # For 'summarize' action: uses a small/fast model to compress
```

**3. Alerter — Keyword & Pattern Alerts**

Fires webhooks or logs alerts when specific content patterns appear in requests or responses. Useful for compliance monitoring without blocking.

```yaml
alerter:
  enabled: true
  rules:
    - name: production_access
      match: "production|prod-db|master-password"
      direction: request  # request | response | both
      action: log         # log | webhook
      webhook_url: ""
    - name: large_data_export
      match_size: 100000  # bytes
      direction: response
      action: webhook
      webhook_url: "https://hooks.slack.com/..."
```

#### Configuration Model

Middleware is configured per-server in the web UI, with global defaults:

```sql
CREATE TABLE middleware_configs (
    id          TEXT PRIMARY KEY,
    server_id   TEXT REFERENCES servers(id),  -- NULL = global default
    middleware  TEXT NOT NULL,                 -- middleware name
    enabled     BOOLEAN DEFAULT TRUE,
    config      TEXT NOT NULL,                -- JSON config
    priority    INTEGER DEFAULT 100,          -- execution order (lower = first)
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(server_id, middleware)
);
```

**Web UI:**
- Server detail page gets a "Middleware" tab
- Toggle built-in middleware on/off per server
- Configure patterns, thresholds, actions
- View middleware-generated alerts/logs

#### Comma Compliance Integration (Phase 8)

MCP Wrangler provides the open-source traffic interception infrastructure. [Comma Compliance](https://commacompliance.com) provides the intelligence layer — policy engines, compliance rule libraries, audit trails, and enterprise reporting.

**Integration model: MCP Wrangler as the enforcement point, Comma Compliance as the policy source.**

```
┌──────────────────────┐         ┌──────────────────────────┐
│   MCP Wrangler       │         │   Comma Compliance       │
│   (open source)      │◄───────►│   (commercial SaaS)      │
│                      │         │                          │
│  • Traffic proxy     │  Sync   │  • Policy engine         │
│  • Middleware engine  │◄───────►│  • Compliance rules      │
│  • Pattern matching   │  API   │  • Industry templates    │
│  • Block/redact/alert│         │  • Audit trail           │
│  • Local enforcement  │────────►│  • Analytics dashboard   │
│                      │  Events │  • Incident management   │
│  Free, self-hosted   │         │  • SOC2/HIPAA/PCI reports│
└──────────────────────┘         └──────────────────────────┘
```

**How it works:**

1. **Policy sync**: Comma Compliance pushes compliance policies to MCP Wrangler via API. Policies are expressed as middleware configurations (patterns, rules, actions). Wrangler stores them locally and enforces them even if the Comma Compliance service is unreachable.

2. **Event streaming**: MCP Wrangler streams audit events to Comma Compliance — what was accessed, what was redacted, what was blocked, by whom, when. No raw content is sent unless the policy explicitly requires it (configurable).

3. **Compliance middleware**: A special `comma-compliance` middleware that:
   - Fetches and caches policies from the Comma Compliance API
   - Evaluates each request/response against the active policy set
   - Reports violations and enforcement actions back to the service
   - Falls back to cached policies if the service is unreachable

```go
// CommaComplianceMiddleware bridges MCP Wrangler to the Comma Compliance service.
type CommaComplianceMiddleware struct {
    apiURL    string
    apiKey    string
    policies  *PolicyCache   // locally cached, synced periodically
    eventChan chan AuditEvent // buffered channel for async event delivery
}
```

**Configuration:**

```toml
[comma_compliance]
enabled = false
api_url = "https://api.commacompliance.com/v1"
api_key = ""
org_id = ""
sync_interval = "5m"        # how often to pull policy updates
event_buffer_size = 1000    # buffer events if service is temporarily unreachable
send_content = false        # never send raw tool content by default
```

**Web UI integration:**
- Settings page: "Comma Compliance" section with API key entry and connection status
- Server detail: "Compliance" badge showing policy coverage
- Dashboard: compliance summary (violations, blocks, alerts in last 24h)

**Business model alignment:**
- MCP Wrangler is free, open-source, self-hosted — drives adoption
- Basic middleware (sanitizer, sizer, alerter) works standalone — immediate value
- Comma Compliance adds enterprise features: managed policies, audit trails, compliance reporting, multi-tenant management
- Organizations start with MCP Wrangler, graduate to Comma Compliance when they need compliance automation
- The open-source middleware interface means competitors can also build on the platform, but Comma Compliance has the first-mover advantage and deepest integration

**Open questions for Comma Compliance integration:**
- Should MCP Wrangler phone home to Comma Compliance for telemetry/usage stats? (Probably not — keep open source clean)
- Should the Comma Compliance middleware be a separate binary/plugin, or compiled into MCP Wrangler behind a build tag?
- How do we handle the free→paid upgrade path in the UI? Subtle "upgrade" prompts? Feature comparison?
- Should Comma Compliance policies be able to override local middleware config? (Enterprise admin override vs. local autonomy)
- Multi-tenant: one MCP Wrangler instance serving multiple orgs, each with their own Comma Compliance policy set?
- What compliance frameworks do we target first? SOC2, HIPAA, PCI-DSS, GDPR?
- Should the event stream include token counts for billing/usage tracking?

### Phase 9: Platform Abstraction & Agent Traffic Proxy

Three interrelated changes that evolve MCP Wrangler from a Docker-specific MCP proxy into a broader AI agent traffic management platform. Each sub-phase was pressure-tested for architectural flaws and descoped where the original plan was overambitious.

#### 9A: External Identity Providers (OIDC)

**Problem:** Every MCP Wrangler instance manages its own user database. In teams, this means another password to manage. Enterprise deployments need SSO.

**Solution:** Federated login via OpenID Connect (covers Google Workspace, Microsoft Entra/M365, Okta, Auth0, Keycloak, and any OIDC-compliant IdP). No SAML — orgs that need SAML can use an OIDC broker (Keycloak, Dex, Authentik, Azure AD as broker) and Wrangler stays an OIDC Relying Party only. This avoids owning XML signatures, clock skew, metadata parsing, and SAML session index handling.

**Login flow:**

```
User clicks "Log in with Google" (or M365, etc.)
    │
    ▼
MCP Wrangler redirects to IdP authorize endpoint
    │  (with state, nonce, redirect_uri)
    ▼
User authenticates at IdP
    │
    ▼
IdP redirects back to /auth/oidc/callback
    │  (with authorization code)
    ▼
Wrangler exchanges code for tokens
    │  → validates id_token
    │  → extracts claims (email, name, groups)
    ▼
JIT provisioning: create or update local user record
    │  → map IdP groups to wrangler roles/access levels
    │  → link identity to user_identities table
    ▼
Create session cookie, redirect to dashboard
```

**Configuration:**

```toml
# Multiple providers supported. First one becomes the default login button.
[[auth.oidc]]
name = "google"
display_name = "Google Workspace"
issuer = "https://accounts.google.com"
client_id = "..."
client_secret = "encrypted:..."
scopes = ["openid", "email", "profile"]
# Optional: restrict to specific domain
allowed_domains = ["yourcompany.com"]
# Claim used for group extraction (varies by IdP)
group_claim = "groups"  # or "roles", "hd", custom claim
# Map IdP groups/roles to wrangler access levels
[auth.oidc.role_mapping]
"admin@yourcompany.com" = "admin"
"mcp-admins" = "admin"       # group claim
"*" = "write"                 # default for all authenticated users

[[auth.oidc]]
name = "microsoft"
display_name = "Microsoft 365"
issuer = "https://login.microsoftonline.com/{tenant-id}/v2.0"
client_id = "..."
client_secret = "encrypted:..."
scopes = ["openid", "email", "profile"]
# Azure AD often hits group overage (>200 groups) — may need Graph API fallback
group_claim = "groups"
```

**User model changes:**

```sql
-- Separate identity table supports multiple providers per user
-- and avoids the "Google first, then M365 = two users" problem.
CREATE TABLE user_identities (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_type   TEXT NOT NULL,     -- 'local', 'oidc'
    issuer          TEXT,              -- OIDC issuer URL
    subject         TEXT,              -- OIDC subject claim (stable, unique per user per IdP)
    email           TEXT,
    email_verified  BOOLEAN DEFAULT FALSE,
    display_name    TEXT,
    avatar_url      TEXT,
    raw_claims      TEXT,              -- JSON snapshot for debugging
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_login_at   DATETIME,
    UNIQUE(issuer, subject)
);

CREATE INDEX idx_user_identities_user ON user_identities(user_id);

-- Deprovisioning support
ALTER TABLE users ADD COLUMN email TEXT;
ALTER TABLE users ADD COLUMN disabled_at DATETIME;
    -- When set, all sessions + API keys are rejected
ALTER TABLE users ADD COLUMN last_idp_login_at DATETIME;
```

**Deprovisioning (the hard part):**

JIT provisioning alone doesn't handle offboarding. If a user is removed from the IdP, Wrangler won't know until the next login. Options in order of complexity:

1. **Baseline (ship this first):** Roles update on login. Add `disabled_at` to users table. Enforce on every request (UI session check, API key validation, proxy auth). Admin can manually disable users. API keys fail immediately for disabled users.
2. **Better:** Short session TTL (e.g., 8h instead of 30d) for OIDC users — forces re-auth, which fails if IdP access is revoked.
3. **Enterprise (later):** SCIM provisioning endpoint — IdP pushes user create/disable/delete events. This is the real answer for enterprise offboarding but is a significant feature.

**Multi-provider identity linking:**

A user who logs in with Google and then M365 should NOT create two separate user accounts. But auto-merging by email is an account takeover risk (emails can be unverified, reused, or change ownership).

Rules:
- `user_identities` table keyed by `(issuer, subject)` — each identity links to one `users` row
- First login from a new IdP creates a new user
- Account linking is **explicit**: logged-in user clicks "Link another provider" which adds a second row in `user_identities` pointing to the same `user_id`
- Auto-linking (if enabled) requires `email_verified=true` + email matches existing user + issuer is in allowlist. Still risky — off by default.

**Service accounts for CI/automation:**

Humans use OIDC browser flow. CI systems can't. Options:

- **Wrangler-native service accounts (ship this):** Separate principal type in `users` table (`role = 'service'`). Admin creates them via UI, gets a long-lived API key. No IdP required. Scoped by access level like normal users.
- **OIDC client credentials (later):** Machine-to-machine tokens — but many IdPs lock this down, and you need per-tenant client registration.
- **Workload identity federation (much later):** GitHub Actions OIDC → token exchange. Powerful but a whole project.

**Role resolution debugging:**

Group claims are unreliable across IdPs (Azure AD group overage, Okta config differences, Google requiring extra setup). Admins will ask "why does Alice have admin?" A TOML mapping with no visibility is painful.

Ship with:
- A "Role debug" screen: shows raw claims (redacted sensitive fields), matched mapping rules, resulting role
- Support multiple group claim strategies: `claim=groups`, `claim=roles`, regex matches
- Log claim hash on each login for audit trail

**Key behaviors:**
- Local password auth remains for admin recovery and air-gapped deployments
- Login page shows configured IdP buttons + optional local login form
- API keys are independent of session lifecycle (but fail if user is disabled)
- Consider `api_keys.expires_at` for time-limited keys
- Device auth flow (mcp-sync) still works — user approves via browser using whatever login method they have
- Admin can disable local auth entirely (force SSO)

**New endpoints:**
- `GET /auth/oidc/{provider}` — initiate OIDC flow (redirect to IdP)
- `GET /auth/oidc/callback` — handle IdP callback
- `POST /auth/oidc/link` — link additional provider to current user (requires session)
- Admin UI: "Identity Providers" settings page + "Role debug" screen

**Dependencies:**
- `github.com/coreos/go-oidc/v3` — OIDC discovery, token validation, JWK handling

---

#### 9B: Container Runtime Abstraction

**Problem:** MCP Wrangler is hardwired to the Docker socket. This limits deployment to machines with Docker installed and prevents using managed container services.

**Descoped reality check:** Supporting ACI/ECS/K8s is not "a runtime adapter" — it's a product line (credentials, networking, RBAC, cost controls, drift handling, image registries). Cloud runtimes also can't do Docker-style exec-attach for stdio servers, and networking/addressing varies wildly between providers. The original plan for a true "sidecar" was architecturally flawed — a sidecar process in another container *cannot* read the main container's stdin/stdout.

**Phase 9B scope: Interface extraction + Docker remote only.** Cloud backends are deferred to a future phase with validated demand.

**Runtime interface (capability-based):**

Rather than pretending all runtimes are equivalent, the interface explicitly declares capabilities:

```go
// ContainerRuntime manages the lifecycle of MCP server containers.
type ContainerRuntime interface {
    // Name returns the runtime identifier (e.g., "docker-local", "docker-remote").
    Name() string

    // Capabilities returns what this runtime supports.
    Capabilities() RuntimeCapabilities

    // PullImage ensures the image is available.
    PullImage(ctx context.Context, ref string) error

    // BuildImage builds an image from a Dockerfile.
    // Returns ErrNotSupported if !Capabilities().Build.
    BuildImage(ctx context.Context, opts BuildOptions) (string, error)

    // CreateContainer creates a container but doesn't start it.
    CreateContainer(ctx context.Context, opts ContainerOptions) (ContainerID, error)

    // StartContainer starts a created container.
    StartContainer(ctx context.Context, id ContainerID) error

    // StopContainer stops a running container.
    StopContainer(ctx context.Context, id ContainerID) error

    // RemoveContainer removes a stopped container.
    RemoveContainer(ctx context.Context, id ContainerID) error

    // AttachStdio returns an io.ReadWriteCloser connected to the
    // container's stdin/stdout for stdio-mode MCP servers.
    // Returns ErrNotSupported if !Capabilities().StdioAttach.
    AttachStdio(ctx context.Context, id ContainerID) (io.ReadWriteCloser, error)

    // ContainerAddr returns the network address (host:port) for an
    // HTTP-mode MCP server running in the container.
    ContainerAddr(ctx context.Context, id ContainerID, port int) (string, error)

    // ContainerStatus returns the current state of a container.
    ContainerStatus(ctx context.Context, id ContainerID) (ContainerState, error)

    // Ping verifies connectivity to the runtime.
    Ping(ctx context.Context) error
}

type RuntimeCapabilities struct {
    StdioAttach bool // Can attach to container stdin/stdout (Docker only)
    Build       bool // Can build images from Dockerfile
    ExecHealth  bool // Can exec commands for health checks
}

var ErrNotSupported = errors.New("operation not supported by this runtime")
```

**Docker backend** (current behavior, refactored behind the interface):
- `StdioAttach: true`, `Build: true`, `ExecHealth: true`
- This is a refactor of `internal/docker/` to implement the interface
- Supports both local socket and remote TCP (`docker -H tcp://...`)

**Configuration:**

```toml
[runtime]
type = "docker"  # only "docker" for now

[runtime.docker]
host = "unix:///var/run/docker.sock"  # or "tcp://remote-host:2376"
api_version = ""                       # auto-detect or pin
# TLS for remote Docker (optional)
tls_ca = ""
tls_cert = ""
tls_key = ""
```

**Implementation order:**
1. Extract `ContainerRuntime` interface from current Docker code (refactor, no behavior change)
2. Move existing Docker logic into `internal/runtime/docker/` implementing the interface
3. Add Docker-remote support (same API, different transport — mostly config)
4. Update proxy manager / server store to use the interface instead of direct Docker calls

**Future cloud runtimes (deferred, documented for planning):**

When there's validated demand, cloud runtimes would need:
- **Wrapper image, not sidecar:** For stdio servers, auto-build a derived image: `FROM userimage`, copy in `wrangler-bridge` binary, set entrypoint to `wrangler-bridge -- <original-cmd>`. The bridge runs as the parent process of the MCP server in the *same* container and exposes an HTTP endpoint. This ties into Phase 6's build system.
- **Wrangler Agent:** For cloud runtimes, Wrangler likely needs an agent/controller deployed *in* the cloud network to handle addressing, port-forward/tunnel, and health checks. A single binary on a laptop can't reliably reach containers in a VPC.
- **Require prebuilt images:** No auto-build on cloud runtimes initially. Users push images to their registry (ECR, ACR, etc.).
- **Health checks:** Unify at the Wrangler level ("can I complete an MCP ping within N ms?") rather than relying on runtime-specific exec/health mechanisms.
- **Cost controls:** Idle timeouts + auto-stop + per-user container quotas.

---

#### 9C: HTTP Proxy Mode — Agent Traffic Interception

**Problem:** MCP Wrangler sees MCP tool calls but is blind to everything else an AI agent does on the network — `curl`, `npm install`, `git clone`, `pip install`, API calls from generated code. There's no unified view of agent network activity.

**Opportunity:** [Claude Code's sandbox](https://code.claude.com/docs/en/sandboxing) routes all network traffic through a configurable proxy (`sandbox.network.httpProxyPort`). If that proxy is MCP Wrangler, you get a single enforcement point for both MCP and HTTP traffic.

> **Hard prerequisite:** Prototype this with Claude Code's actual sandbox before committing to implementation. The sandbox may run its own internal proxy with chaining behavior that limits what an external proxy can do. If `npm install` / `git clone` can't reliably route through our proxy, this phase collapses.

**Scope: localhost-only, single-user, metadata-only.** The sandbox config only accepts a port, not auth credentials. This means the proxy cannot authenticate connections via headers or tokens. The security boundary is "only processes on this machine can reach localhost:8081." Multi-user or network-exposed proxy is a non-starter without additional auth mechanisms.

**Architecture:**

```
Claude Code (sandboxed, same machine)
    │
    ├── MCP tool calls ──► /mcp/{server-name} ──► MCP Proxy (existing)
    │                           │
    │                           ├─ Auth (API key)
    │                           ├─ RBAC
    │                           ├─ Middleware pipeline
    │                           └─ Backend server
    │
    └── HTTP requests ──► localhost:8081 ──► HTTP Proxy (new)
         (curl, npm, git, etc.)     │
                                    ├─ Domain allowlist
                                    ├─ Request metadata logging
                                    ├─ Egress policy (allow/deny/log)
                                    └─ Upstream (direct or chained proxy)

Same admin UI, unified audit trail.
```

**How it works:**

MCP Wrangler runs an HTTP forward proxy bound to `127.0.0.1` on a separate port (default 8081). Claude Code's sandbox configuration points at it:

```json
{
  "sandbox": {
    "network": {
      "httpProxyPort": 8081
    }
  }
}
```

**Proxy behaviors:**

1. **HTTP CONNECT** (HTTPS traffic): Wrangler receives `CONNECT example.com:443`, checks domain against allowlist, then tunnels the TLS connection. Logs domain, port, bytes transferred, timing, and outcome. **No content inspection** — domain-level filtering and metadata only.

2. **Plain HTTP**: Wrangler receives the full request, can inspect/log headers and domain, then forwards upstream. Body logging is off by default.

3. **TLS MITM: Punted entirely.** The complexity (CA generation, cert distribution, per-runtime trust stores, HSTS/pinning failures, HTTP/2 handling, WebSockets, gRPC, governance) is not justified for Phase 9. If Comma Compliance needs content inspection later, it would be a separate dedicated feature.

**Domain allowlist:**

```toml
[proxy]
enabled = false  # opt-in
listen = "127.0.0.1:8081"  # localhost only — NEVER bind to 0.0.0.0
mode = "allowlist"  # "allowlist" (default) or "passthrough" (log-only)

# Upstream proxy chaining (for corporate proxy environments)
upstream_proxy = ""  # e.g., "http://corporate-proxy.internal:8080"

# Default allowed domains for common dev workflows
default_allowed = [
    "registry.npmjs.org",
    "pypi.org",
    "files.pythonhosted.org",
    "github.com",
    "api.github.com",
    "rubygems.org",
    "pkg.go.dev",
    "proxy.golang.org",
    "sum.golang.org",
]

# Blocked IP ranges (always enforced, even in passthrough mode)
# These prevent exfiltration to internal networks and cloud metadata services
blocked_ranges = [
    "10.0.0.0/8",
    "172.16.0.0/12",
    "192.168.0.0/16",
    "169.254.0.0/16",     # link-local + cloud metadata (AWS/GCP/Azure)
    "127.0.0.0/8",        # localhost
    "fd00::/8",            # IPv6 ULA
]
```

**Egress policy enforcement:**
- CONNECT to **IP literals** (bypassing domain policy): deny by default, require explicit allowlist
- Cloud metadata endpoints (`169.254.169.254`): always blocked
- RFC1918/link-local/multicast: always blocked (prevents lateral movement)
- DNS resolution: resolve before connecting, enforce IP policy on resolved addresses

**Database schema:**

```sql
CREATE TABLE proxy_rules (
    id          TEXT PRIMARY KEY,
    domain      TEXT NOT NULL,              -- e.g., "*.github.com", "api.openai.com"
    action      TEXT NOT NULL DEFAULT 'allow',  -- 'allow', 'deny', 'log'
    created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(domain)
);

-- Extend request_logs for HTTP proxy events
ALTER TABLE request_logs ADD COLUMN source TEXT DEFAULT 'mcp';
    -- 'mcp' = MCP proxy, 'http' = HTTP proxy
ALTER TABLE request_logs ADD COLUMN domain TEXT;
```

**Logging performance (SQLite will get crushed by HTTP volume):**

Package installs generate hundreds/thousands of HTTP requests. Naive per-request logging will bottleneck on SQLite locks.

Mitigations:
- **Async batched logging:** Buffer events in a channel, flush to DB in batches (e.g., every 100 events or 1 second)
- **Aggregate mode:** Store per-domain totals (request count, bytes, last seen) + sampled exemplars, not every request
- **Retention limits:** Auto-purge HTTP proxy logs older than N days (configurable, default 7d). MCP logs have separate, longer retention.
- **No body buffering:** Proxy streams data through — never buffers request/response bodies in memory

**Middleware: separate chains, shared event envelope:**

The MCP middleware pipeline (Sanitizer, Sizer, Alerter) operates on JSON-RPC messages. HTTP proxy traffic is arbitrary binary data. Forcing them through the same pipeline creates mismatches ("sanitize an npm tarball?").

Instead:
- Define a common **audit event envelope** (identity, domain, timestamp, bytes, policy decision, source)
- MCP middleware chain stays as-is for MCP traffic
- HTTP proxy gets its own policy chain: domain allowlist → IP policy → rate limits → alerter (domain pattern alerts)
- **Shared components:** Alerter (pattern matching on domains/URLs), audit logger, dashboard aggregation
- **MCP-only components:** Sanitizer, Sizer (these inspect JSON-RPC payloads)

**Protocol coverage (be honest about gaps):**

The HTTP proxy catches:
- HTTP/1.1 plain text requests
- HTTPS via CONNECT (metadata only — domain, port, bytes, timing)
- WebSockets *if* properly upgraded through CONNECT

It will NOT catch:
- `git+ssh`, raw SSH, TCP sockets, UDP, DNS-over-HTTPS (depending on config)
- gRPC typically tunnels over CONNECT (metadata only)
- Tools that ignore proxy environment variables or use custom network stacks
- HTTP/3 (QUIC) — often bypasses HTTP proxies entirely

Marketing/UI should say "HTTP(S) egress monitoring" not "all agent traffic."

**Corporate proxy chaining:**

Many developers are already behind a corporate proxy. The Wrangler proxy must support:
- Static upstream proxy configuration (`upstream_proxy` in TOML)
- Passing through upstream proxy auth if needed
- PAC files: **not supported** initially (non-trivial to implement)
- Document: "If you're behind a corporate proxy, set `upstream_proxy` to chain through it"

**Web UI additions:**
- Dashboard: "HTTP Proxy" card showing request counts, top domains, blocked requests
- Settings: Proxy on/off toggle, port, mode, domain allowlist editor
- Logs: Combined MCP + HTTP view with source filter tab

**Implementation order:**
1. **Prototype with Claude Code sandbox** — verify traffic actually routes through external proxy
2. Basic HTTP CONNECT proxy with domain allowlist, localhost-only
3. Blocked IP ranges enforcement (RFC1918, metadata endpoints)
4. Async batched request metadata logging
5. Web UI for proxy rules and log viewing
6. Alerter integration (domain pattern alerts shared with MCP alerter)
7. Upstream proxy chaining support
8. mcp-sync integration for auto-configuring sandbox settings (stretch)

---

#### How 9A/9B/9C reinforce each other

These three pieces aren't just parallel features — they compound:

- **9A + 9C**: Enterprise SSO login → the HTTP proxy logs are tied to real federated identities → compliance audit trail shows "alice@company.com ran `npm install` targeting these domains" not "API key xyz"
- **9A + 9B**: SSO users get scoped access → OIDC claims determine access levels → future cloud runtimes could route by role (dev → local Docker, prod → managed)
- **9B + 9C**: Container runtime interface → proxy manager decoupled from Docker → when cloud backends land, the HTTP proxy provides the same network visibility regardless of where containers run

The combination evolves MCP Wrangler from "Docker MCP proxy" toward "AI agent traffic gateway."

#### Sequencing and risk

**Ship order:** 9A (OIDC) → 9B (interface extraction) → 9C (HTTP proxy)

Rationale:
- 9A is self-contained, high value, and doesn't depend on 9B or 9C
- 9B is a refactor (improves code quality regardless of cloud backends)
- 9C has the highest risk (sandbox integration is unproven) — do it last so the prototype validates before heavy investment

**Cross-cutting concern:** If 9C produces massive audit volume, it will stress the same DB/UI/logging systems that 9A (security audit) and 9B (runtime telemetry) rely on. Plan retention, backpressure, and async logging *before* adding the HTTP proxy firehose.

**Enterprise scope creep warning:** If you target enterprises with SSO + proxy interception + cloud runtimes, you're implicitly signing up for SCIM/deprovisioning, service accounts, policy engine, and tenancy boundaries. If you don't want that, keep Phase 9 scoped to "small team / local-first" and let Comma Compliance handle the enterprise layer.

---

## Bugfixes & Small Improvements

### Connection Examples: Use Base URL Scheme

**Status:** Bug — needs fix

The server detail page connection examples (`claude mcp add`, Claude Desktop JSON) hardcode `http://` prefix. When `MCP_WRANGLER_BASE_URL` is set (e.g., `https://mcp.home.jeremiah.church`), the examples should use that URL directly instead of `http://{{.Host}}`.

**Fix:** Pass `BaseURL` (from `cfg.PublicBaseURL()`) to the template instead of `r.Host`. Update `server_detail.html` to use `{{.BaseURL}}` as the full URL prefix.

**Files:**
- `internal/web/handlers.go` — pass `BaseURL` to template data
- `internal/web/templates/server_detail.html` — use `{{.BaseURL}}/mcp/{{.Server.Name}}`

---

## Open Questions & Considerations

### Stdio Concurrency
The MCP spec doesn't explicitly address concurrent requests over stdio. Most stdio servers are single-threaded and process one request at a time. For the POC, serializing requests per container (with a mutex + queue) is safest. If throughput matters later, we can spin up multiple container instances per server.

### Credential Storage
For POC: AES-256-GCM encryption with a key from config/env var. Credentials are encrypted at rest in SQLite. This is "good enough" for a self-hosted tool. A vault integration can come later.

### Container Image Management
Pre-built images only (`docker pull`) for the POC. Building from source (Dockerfiles, repos) adds significant complexity and can be added later. See Phase 6 for the auto-build plan.

### MCP Protocol Version Compatibility
The current MCP spec (2025-03-26) uses Streamable HTTP. Older servers may use the deprecated SSE transport. MCP Wrangler should attempt Streamable HTTP first, fall back to SSE per the spec's backwards compatibility guidance.

### Middleware Performance
The middleware pipeline adds latency to every proxied request. Design considerations:
- Middleware should be fast (microseconds, not milliseconds) for pattern matching
- Regex patterns should be pre-compiled at config load time
- Content summarization (via LLM) should be async/optional — never block by default
- Middleware that calls external services (webhooks, Comma Compliance API) should be non-blocking

### Open Source Governance
MCP Wrangler under Comma Compliance org branding:
- License: MIT or Apache 2.0 (permissive, encourages adoption)
- GitHub org: `comma-compliance/mcp-wrangler` (or keep `JeremiahChurch` and add org attribution?)
- README: "Built by [Comma Compliance](https://commacompliance.com)" with clear separation between open-source core and commercial offering
- Contributor agreement: standard CLA or DCO?

---

## Maybe Later: Ideas from Competitive Analysis

Patterns and concepts observed in other MCP proxy projects (March 2026). Not committed to any phase - revisit when scoping future work.

**Sources:** [remote-mcp-adapter](https://github.com/aakashh242/remote-mcp-adapter) (v0.3.0), [PolicyLayer/Intercept](https://github.com/policylayer/intercept) (v1.1.0), [mcp-zero-trust-proxy](https://github.com/AnobleSCM/mcp-zero-trust-proxy) (v1.0.1)

### Per-Session Backend Isolation
**From:** mcp-zero-trust-proxy, remote-mcp-adapter
**Gap:** Wrangler keeps one backend per server (one shared stdio bridge, one remote sessionID). All three competitors isolate backends per auth context or MCP session. This matters for multi-user deployments where concurrent users share a server.
**Overlap:** Partially addressed if 9A (OIDC) ships - scoped sessions become natural.

### Request Body Size Cap
**From:** mcp-zero-trust-proxy
**Gap:** Wrangler does unbounded `io.ReadAll` in the HTTP handler. A `MaxBodySize` check before parse is trivial and prevents OOM on malicious payloads.
**Effort:** Low - could ship standalone.

### Declarative Policy Middleware (tools/call Rules Engine)
**From:** PolicyLayer/Intercept
**Gap:** Wrangler's Sanitizer does regex match/redact/block, but Intercept has a real policy language: nested-path conditions on tool arguments (`arguments.path startsWith /safe`), wildcard rules, deny/warn actions, per-tool rate limits, stateful counters with `increment_from`, optional Redis-backed shared state. This is significantly more expressive than regex patterns.
**Overlap:** Extends the existing middleware registry in `internal/middleware/`. Could be a new middleware type alongside Sanitizer/Sizer/Alerter.

### Policy Scan & Auto-Generation
**From:** PolicyLayer/Intercept
**Gap:** Intercept's `scan` command generates starter policies from live tool catalogs, grouped by read/write/destructive risk. Wrangler already enumerates server endpoints - this would use that data to bootstrap middleware configs rather than requiring manual setup.
**Pairs with:** Declarative policy middleware above.

### Tool Definition Pinning & Drift Detection
**From:** remote-mcp-adapter
**Gap:** Snapshot `tools/list` on first enumeration, alert when upstream schemas change. Defends against prompt injection via modified tool descriptions and catches unexpected upstream changes.
**Overlap:** Health monitoring already probes servers periodically - drift detection could piggyback on that.

### Tool Metadata Sanitization
**From:** remote-mcp-adapter
**Gap:** Scrub model-visible tool descriptions/schema text in `tools/list` responses before forwarding to clients. Defense against prompt injection embedded in tool metadata by upstream servers.
**Effort:** Low - hooks into the same place Wrangler already filters tools.

### Decision-Oriented Audit Logging
**From:** PolicyLayer/Intercept
**Gap:** Wrangler's middleware_events table logs request summaries. Intercept logs structured decision events: allowed/denied, matched rule ID, hashed arguments (not raw payloads by default), config reload events. Better for compliance review.
**Overlap:** Pairs naturally with the Comma Compliance integration (Phase 8).

### Hot Reload for Middleware Config
**From:** PolicyLayer/Intercept
**Gap:** Middleware/policy changes require server restart. Intercept validates config changes and applies them live. Wrangler's registry factory pattern could support this with a rebuild-pipeline-on-change mechanism.

### Session-Scoped File Staging (upload/artifact handles)
**From:** remote-mcp-adapter
**Gap:** Inject an upload helper tool into stdio servers, issue signed short-lived upload URLs, rewrite `upload://` handles to container-visible paths, capture output files as `artifact://` resources with download endpoints. Solves file exchange with Docker stdio servers.
**Effort:** High - new transport layer concern.

---

## Dependencies (Go)

| Package | Purpose |
|---------|---------|
| `github.com/moby/moby/client` | Docker API client |
| `github.com/mattn/go-sqlite3` | SQLite driver |
| `github.com/gorilla/mux` or `net/http` | HTTP routing (stdlib may suffice) |
| `github.com/BurntSushi/toml` | Config parsing |
| `golang.org/x/crypto/bcrypt` | Password hashing |
| `github.com/google/uuid` | UUIDs |
