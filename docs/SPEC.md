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
    "image": "ghcr.io/jeremiahchurch/pfsense-mcp-server:latest",
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
    "image": "ghcr.io/jeremiahchurch/pfsense-mcp-server:latest",
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
    "url": "http://192.168.1.100:9583/private_zctpwlX7ZkIAr7oqdfLPxw",
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

### Phase 5: Stretch Goals
22. Request logging to DB
23. Connection config generation (Claude Desktop, Claude Code snippets)
24. Access logs dashboard with basic charts
25. Consolidated stderr/log capture for managed servers

---

## Open Questions & Considerations

### Stdio Concurrency
The MCP spec doesn't explicitly address concurrent requests over stdio. Most stdio servers are single-threaded and process one request at a time. For the POC, serializing requests per container (with a mutex + queue) is safest. If throughput matters later, we can spin up multiple container instances per server.

### Credential Storage
For POC: AES-256-GCM encryption with a key from config/env var. Credentials are encrypted at rest in SQLite. This is "good enough" for a self-hosted tool. A vault integration can come later.

### Container Image Management
Pre-built images only (`docker pull`) for the POC. Building from source (Dockerfiles, repos) adds significant complexity and can be added later.

### MCP Protocol Version Compatibility
The current MCP spec (2025-03-26) uses Streamable HTTP. Older servers may use the deprecated SSE transport. MCP Wrangler should attempt Streamable HTTP first, fall back to SSE per the spec's backwards compatibility guidance.

---

## Dependencies (Go)

| Package | Purpose |
|---------|---------|
| `github.com/docker/docker/client` | Docker API client |
| `github.com/mattn/go-sqlite3` | SQLite driver |
| `github.com/gorilla/mux` or `net/http` | HTTP routing (stdlib may suffice) |
| `github.com/BurntSushi/toml` | Config parsing |
| `golang.org/x/crypto/bcrypt` | Password hashing |
| `github.com/google/uuid` | UUIDs |
