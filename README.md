# MCP Wrangler

A management proxy for [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) servers. MCP Wrangler sits between your AI tools (Claude Code, Claude Desktop, etc.) and your MCP servers, providing a single authenticated endpoint that handles Docker container lifecycle, health monitoring, and multi-transport proxying.

## Features

- **Unified proxy** — expose all your MCP servers through one endpoint (`/mcp/{server-name}`)
- **Multi-transport** — supports stdio (Docker), HTTP (Docker or external), and remote (SSE/Streamable HTTP) backends
- **Docker lifecycle management** — automatically starts, stops, and monitors containers for stdio and HTTP MCP servers
- **Web UI** — manage servers, users, and API keys from a browser dashboard
- **API key authentication** — issue Bearer tokens to control access to proxied MCP servers
- **Endpoint enumeration** — discovers tools, prompts, and resources exposed by each MCP server
- **Access tiers** — per-endpoint risk-based access control (read/write/admin) with auto-classification
- **OAuth support** — PKCE authorization flows for remote servers (e.g., Sentry MCP), with automatic token refresh
- **Request logging** — per-endpoint call counts, error tracking, and recent activity dashboard
- **Health monitoring** — periodic health checks with automatic recovery for remote and external HTTP servers
- **Credential encryption** — AES-GCM encryption at rest for server configs (tokens, API keys, env vars)
- **Rate limiting** — per-user token bucket rate limiting on proxy endpoints

## Quick Start (Docker Compose)

```bash
# Clone and configure
git clone https://github.com/JeremiahChurch/mcp-wrangler.git
cd mcp-wrangler
cp .env.example .env
# Edit .env — change the encryption key, session secret, and admin password

# Start
docker compose up -d

# Open the web UI
open http://localhost:8080
```

Log in with username `admin` and the password you set in `.env`.

## Building from Source

Requires Go 1.24+, GCC, and SQLite dev headers (see [CONTRIBUTING.md](CONTRIBUTING.md) for platform-specific instructions).

```bash
make build
./mcp-wrangler --config config.example.toml
```

## Configuration

MCP Wrangler reads a TOML config file and environment variables. See [`config.example.toml`](config.example.toml) for all options.

Key environment variables (also settable in `.env`):

| Variable | Purpose |
|---|---|
| `MCP_WRANGLER_ENCRYPTION_KEY` | Encrypts stored credentials |
| `MCP_WRANGLER_SESSION_SECRET` | Signs web UI session cookies |
| `MCP_WRANGLER_ADMIN_PASSWORD` | Initial admin password (first run only) |
| `MCP_WRANGLER_DB_PATH` | SQLite database path |

## Architecture

MCP Wrangler supports three server types:

- **stdio** — runs an MCP server in a Docker container, communicating over stdin/stdout (JSON-RPC). Best for servers that use the stdio transport (e.g., Python FastMCP servers).
- **http** — runs an MCP server in a Docker container that exposes an HTTP port. MCP Wrangler proxies requests to the container.
- **remote** — proxies to an externally hosted MCP server (SSE or Streamable HTTP). No container management — just auth and proxying.

All server types are accessed through the same unified endpoint: `POST /mcp/{server-name}`.

## Adding an MCP Server to Claude Code

Once MCP Wrangler is running with servers configured, point Claude Code at the proxy:

```bash
claude mcp add --transport http my-server \
  http://localhost:8080/mcp/my-server \
  --header "Authorization: Bearer YOUR_API_KEY"
```

Generate API keys from the web UI under **API Keys**.

## Documentation

See [`docs/SPEC.md`](docs/SPEC.md) for the full specification, including server configuration examples, proxy behavior, and the access control model.

## License

MCP Wrangler is licensed under the [GNU Affero General Public License v3.0](LICENSE).
