# Arc Relay

An open-source MCP (Model Context Protocol) control plane. Arc Relay sits between your AI tools and MCP servers, providing auth, policy controls, traffic interception, and archiving - not just proxying.

```
AI Clients                Arc Relay                    MCP Servers
 (Claude Code,     +-----------------------+       +----------------+
  Cursor, etc.)    |  Auth & API Keys      |       | Docker stdio   |
       |           |  Middleware Pipeline   |----->| Docker HTTP    |
       +---------->|    Sanitizer (PII)     |      | Remote (OAuth) |
       |  POST     |    Sizer (limits)      |<-----+----------------+
       |  /mcp/    |    Alerter (rules)     |
       |  {name}   |    Archive (webhook)   |
       |           |  Health Monitor        |
       +---------->|  Web UI + REST API     |
                   +-----------------------+
```

## Features

- **Unified proxy** - all MCP servers behind one endpoint (`/mcp/{server-name}`)
- **Middleware pipeline** - bidirectional request/response processing (sanitizer, sizer, alerter, archive)
- **Archive with encryption** - stream tool calls to any webhook, optionally encrypted with NaCl Box
- **Docker lifecycle** - auto-start, stop, health check, and recover containers
- **Multi-transport** - stdio (Docker), HTTP (Docker/external), remote (SSE/OAuth)
- **Auth** - session cookies (web UI) + Bearer API keys (proxy) + OAuth 2.1 (remote servers)
- **Access tiers** - per-endpoint risk-based access control with auto-classification
- **Web UI** - manage servers, users, API keys, middleware, and logs
- **CLI tool** (`arc-sync`) - sync MCP servers to Claude Code projects via `.mcp.json`
- **Health monitoring** - periodic pings with auto-recovery for failed servers

## Quick Start

### Docker Compose

```bash
git clone https://github.com/comma-compliance/arc-relay.git
cd arc-relay
cp .env.example .env
# Edit .env - change encryption key, session secret, and admin password

docker compose up -d
open http://localhost:8080
```

### From Source

Requires Go 1.24+, GCC, and SQLite dev headers.

```bash
make build
./arc-relay --config config.example.toml
```

Log in with username `admin` and the password from your `.env` or config.

## Configuration

Arc Relay reads a TOML config file with environment variable overrides. See [`config.example.toml`](config.example.toml).

| Variable | Purpose |
|---|---|
| `ARC_RELAY_ENCRYPTION_KEY` | Encrypts stored credentials (generate: `openssl rand -hex 32`) |
| `ARC_RELAY_SESSION_SECRET` | Signs web UI session cookies |
| `ARC_RELAY_ADMIN_PASSWORD` | Initial admin password (first run only) |
| `ARC_RELAY_DB_PATH` | SQLite database path (default: `arc-relay.db`) |
| `ARC_RELAY_BASE_URL` | Public URL for OAuth callbacks |
| `ARC_RELAY_LLM_API_KEY` | Anthropic API key for tool context optimization (optional) |
| `ARC_RELAY_LLM_MODEL` | LLM model for optimization (default: `claude-haiku-4-5-20251001`) |

## User Onboarding

Arc Relay supports invite-based onboarding. Admins create invite links from the Users page; recipients click the link, choose a username and password, and immediately receive an API key for CLI access.

**Web UI invites:**
1. Go to the Users page and click "Create Invite"
2. Set the role (admin, user) and access level
3. Share the invite link - it's a one-time use URL that expires

**CLI invites:**
```bash
# Recipient runs this with the invite token from their admin:
arc-sync init https://your-relay:8080 --token INVITE_TOKEN
# They'll be prompted to choose a username and password
```

## CLI Tools

### arc-sync

`arc-sync` manages the connection between Arc Relay and your AI coding tools. It syncs MCP server definitions into `.mcp.json` files for Claude Code, Cursor, and VS Code projects.

**Install:**
```bash
# Download from your relay instance:
curl -fsSL https://your-relay:8080/install.sh | bash

# Or build from source:
CGO_ENABLED=0 go build ./cmd/arc-sync
```

**Commands:**
```bash
arc-sync init <url>       # Configure relay URL and authenticate (device code flow)
arc-sync                  # Interactive sync - add relay servers to current project
arc-sync list             # Show all servers and which are configured locally
arc-sync add <name>       # Add a specific server to the current project
arc-sync remove <name>    # Remove a server from the current project
arc-sync status           # Show configuration and project details
arc-sync server add       # Add a new MCP server to the relay (admin)
arc-sync server remove    # Remove a server from the relay (admin)
arc-sync server start     # Start a stopped server
arc-sync server stop      # Stop a running server
arc-sync setup-claude     # Install Claude Code skill and instructions
arc-sync setup-project    # Add MCP instructions to project .claude/CLAUDE.md
```

**Authentication:** `arc-sync init` uses the device code flow by default. It opens a browser where you log in and approve the CLI. For CI environments, set `ARC_SYNC_URL` and `ARC_SYNC_API_KEY` environment variables.

### dep-scan

`dep-scan` analyzes Go module dependencies for risk signals - staleness, known vulnerabilities (via `govulncheck`), and maintenance indicators.

```bash
CGO_ENABLED=0 go build ./cmd/dep-scan
dep-scan                      # Table output for current directory
dep-scan -json                # JSON report
dep-scan -threshold 7         # Exit non-zero if any dep scores >= 7
dep-scan -skip-vulncheck      # Faster scan, no CVE data
dep-scan -dir /path/to/module # Scan a different module
```

## Device Code Flow (CLI Authentication)

The device code flow lets CLI tools authenticate without handling passwords directly:

1. CLI calls `POST /api/auth/device` and receives a `device_code` and `user_code`
2. User opens the `verification_url` in a browser and logs in
3. User sees the code and clicks "Approve" (or "Deny")
4. CLI polls `POST /api/auth/device/token` with the `device_code`
5. On approval, the CLI receives an API key scoped to that user

This flow is used by `arc-sync init` and can be integrated into any CLI tool.

## Adding Servers to Claude Code

Install the CLI and sync your project:

```bash
arc-sync init https://your-relay:8080
arc-sync add my-server
```

Or add manually:

```bash
claude mcp add --transport http my-server \
  https://your-relay:8080/mcp/my-server \
  --header "Authorization: Bearer YOUR_API_KEY"
```

## Middleware Pipeline

Arc Relay's middleware processes MCP traffic bidirectionally:

| Middleware | Purpose | Actions |
|---|---|---|
| **Sanitizer** | Redact PII and secrets from responses | redact, block |
| **Sizer** | Enforce response size limits | truncate, warn, block |
| **Alerter** | Pattern and size-based alerting | log, webhook |
| **Archive** | Stream requests/responses to a webhook | POST with optional NaCl encryption |

Configure middleware per-server via the web UI or API. The archive middleware supports NaCl Box encryption (X25519 + XSalsa20-Poly1305) for defense-in-depth on top of TLS.

### Middleware Configuration Examples

Middleware is configured per-server as JSON. Below are examples for each type.

**Sanitizer** - redact or block sensitive patterns in responses:
```json
{
  "patterns": [
    {"name": "api_key", "regex": "(?i)(api[_-]?key|secret[_-]?key)\\s*[=:]\\s*\\S+", "action": "redact"},
    {"name": "ssn", "regex": "\\b\\d{3}-\\d{2}-\\d{4}\\b", "action": "redact"},
    {"name": "credit_card", "regex": "\\b\\d{4}[\\s-]?\\d{4}[\\s-]?\\d{4}[\\s-]?\\d{4}\\b", "action": "block"}
  ]
}
```

**Sizer** - enforce response size limits:
```json
{
  "max_response_bytes": 500000,
  "action": "truncate"
}
```
Actions: `truncate` (trim to limit), `warn` (log but pass through), `block` (reject).

**Alerter** - pattern or size-based alerts:
```json
{
  "rules": [
    {"name": "prod_access", "match": "(?i)(production|prod[_-]db)", "direction": "request", "action": "log"},
    {"name": "large_response", "match_size": 100000, "direction": "response", "action": "webhook", "webhook_url": "https://hooks.example.com/alerts"}
  ]
}
```

**Archive** - stream tool calls to a webhook for compliance:
```json
{
  "url": "https://compliance.example.com/webhooks/incoming/arc_webhooks",
  "auth_type": "bearer",
  "auth_value": "your-webhook-token",
  "include": "both",
  "nacl_recipient_key": "base64-encoded-curve25519-public-key"
}
```
`include`: `request`, `response`, or `both`. `nacl_recipient_key` is optional - when set, payloads are encrypted with NaCl Box before delivery.

### Archive Payload Format

The archive middleware sends JSON payloads to the configured webhook URL via HTTP POST. Each payload is an envelope containing the MCP request and/or response:

```json
{
  "version": "v1",
  "source": "arc_relay",
  "phase": "exchange",
  "timestamp": "2026-04-07T12:00:00Z",
  "meta": {
    "server_id": "abc123",
    "server_name": "my-server",
    "user_id": "user-456",
    "client_ip": "10.0.0.1",
    "method": "tools/call",
    "tool_name": "search",
    "request_id": "1"
  },
  "request": {"jsonrpc": "2.0", "method": "tools/call", "params": {}},
  "response": {"jsonrpc": "2.0", "result": {}}
}
```

The `phase` field is `request`, `response`, or `exchange` (both). The `meta` block identifies who made the call, which server handled it, and the MCP method.

### NaCl Encryption for Archive Payloads

When `nacl_recipient_key` is configured, the archive payload is encrypted before delivery using NaCl Box (X25519 + XSalsa20-Poly1305) with an ephemeral sender keypair. The webhook receives a JSON envelope instead of the plaintext payload:

```json
{
  "nonce": "base64-encoded-24-byte-nonce",
  "ciphertext": "base64-encoded-encrypted-payload",
  "sourcePublicKey": "base64-encoded-ephemeral-curve25519-public-key"
}
```

The recipient decrypts using:
1. Their Curve25519 private key (corresponding to the configured `nacl_recipient_key`)
2. The `sourcePublicKey` from the envelope (ephemeral, unique per payload)
3. The `nonce` from the envelope

This provides defense-in-depth on top of TLS - the webhook endpoint cannot read payloads without the private key, even if the transport is compromised.

## Tool Context Optimizer

MCP servers often ship verbose tool definitions that consume excessive LLM context tokens. The Tool Context Optimizer analyzes and compresses these definitions while preserving semantic meaning.

**Without an LLM key:** Each server detail page shows a tool audit card with per-tool size breakdown and estimated token counts. No configuration needed.

**With an LLM key:** Set `ARC_RELAY_LLM_API_KEY` to an [Anthropic API key](https://console.anthropic.com/) to enable LLM-powered optimization. Click "Run Optimization" on any server's detail page to compress tool descriptions. Review the savings, then toggle "Serve optimized tools" to start serving the compressed versions to clients.

## Connect to Comma Compliance Arc

Arc Relay works standalone as a self-hosted MCP control plane. Optionally connect to [Comma Compliance Arc](https://commacompliance.ai) for managed compliance policies, audit trails, and enterprise reporting.

Configure the archive middleware to point at your Comma Compliance webhook endpoint. See the web UI's "Compliance Archive" section for setup.

## Documentation

- [AGENTS.md](AGENTS.md) - AI contributor guide (project structure, key abstractions)
- [CONTRIBUTING.md](CONTRIBUTING.md) - Development setup, PR process
- [SECURITY.md](SECURITY.md) - Vulnerability reporting
- [docs/SPEC.md](docs/SPEC.md) - Full technical specification

## License

Arc Relay is licensed under the [MIT License](LICENSE).

Built by [Comma Compliance](https://commacompliance.ai).
