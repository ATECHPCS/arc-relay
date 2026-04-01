# Arc Relay

An open-source MCP (Model Context Protocol) control plane. Arc Relay sits between your AI tools and MCP servers, providing auth, policy controls, traffic interception, and archiving - not just proxying.

```
AI Clients                Arc Relay                    MCP Servers
 (Claude Code,     +-----------------------+      +----------------+
  Cursor, etc.)    |  Auth & API Keys      |      | Docker stdio   |
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
