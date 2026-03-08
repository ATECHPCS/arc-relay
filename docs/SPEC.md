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

## Dependencies (Go)

| Package | Purpose |
|---------|---------|
| `github.com/moby/moby/client` | Docker API client |
| `github.com/mattn/go-sqlite3` | SQLite driver |
| `github.com/gorilla/mux` or `net/http` | HTTP routing (stdlib may suffice) |
| `github.com/BurntSushi/toml` | Config parsing |
| `golang.org/x/crypto/bcrypt` | Password hashing |
| `github.com/google/uuid` | UUIDs |
