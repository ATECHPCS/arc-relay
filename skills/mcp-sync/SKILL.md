---
name: mcp-sync
description: >
  Manage MCP servers via MCP Wrangler. Use this skill for ANY MCP server operation:
  adding, removing, listing, syncing, configuring, or troubleshooting MCP servers.
  Triggers on: MCP, .mcp.json, server configuration, missing tools, wrangler.
  Do NOT manually edit .mcp.json - always use mcp-sync commands instead.
user-invocable: true
disable-model-invocation: false
allowed-tools: Bash(mcp-sync *)
argument-hint: [list|add <server>|remove <server>|reset|status|setup-project|server add|server remove|server start|server stop]
---

# MCP Server Management via mcp-sync

MCP servers in this environment are managed by MCP Wrangler. **Never edit `.mcp.json` directly** - use `mcp-sync` commands.

## First-run check

Before running any mcp-sync command, check if mcp-sync is configured:

1. Run `mcp-sync status --json 2>/dev/null`. If it fails or returns `{"error": ...}`:
   - Check if `mcp-sync` binary exists: `which mcp-sync`
   - If not installed: tell the user to ask their admin for an install command (invite token), or run `mcp-sync init` if they have credentials
   - If installed but not configured: run `mcp-sync init` to set up

2. If status succeeds but shows missing Claude integration, suggest:
   - `mcp-sync setup-claude` for personal skill installation
   - `mcp-sync setup-project` for team-shared project instructions

## Current project status
!`mcp-sync status --json 2>/dev/null || echo '{"error": "mcp-sync not installed or not configured - run: mcp-sync init"}'`

## Commands

| Command | Description |
|---------|-------------|
| `mcp-sync` | Interactive sync - add new wrangler servers to project |
| `mcp-sync list` | Show all servers (use `--json` for machine-readable) |
| `mcp-sync add <name>` | Add a specific server to this project |
| `mcp-sync remove <name>` | Remove a server from this project |
| `mcp-sync reset` | Clear the skip list for this project |
| `mcp-sync status` | Show config and project details |
| `mcp-sync setup-claude` | Install Claude Code skill and CLAUDE.md instructions |
| `mcp-sync setup-project` | Add MCP instructions to project .claude/CLAUDE.md for team sharing |
| `mcp-sync server add <name> --type remote <url>` | Register a remote MCP server on the wrangler |
| `mcp-sync server add <name> --type stdio --build python --package <pkg>` | Register an auto-build server |
| `mcp-sync server add <name> --type stdio --image <img>` | Register a Docker stdio server |
| `mcp-sync server add <name> --type http --image <img> --port <p>` | Register a Docker HTTP server |
| `mcp-sync server remove <name>` | Delete a server from the wrangler |
| `mcp-sync server start <name>` | Start a wrangler server |
| `mcp-sync server stop <name>` | Stop a wrangler server |

Use `--non-interactive` or `-y` for automation. Use `--dry-run` for preview.

## When to use this skill

Use this skill whenever the conversation involves:
- **MCP** servers, tools, configuration, or `.mcp.json`
- Adding, removing, or configuring tool servers
- Missing tools or "tool not found" errors
- Server health, status, or connectivity issues
- Any mention of wrangler or server management

**Always prefer `mcp-sync` over manually editing `.mcp.json`.**

## Usage from arguments

If arguments are provided, run: `mcp-sync $ARGUMENTS`
Otherwise, run `mcp-sync list` first to show status, then ask the user what they'd like to do.
