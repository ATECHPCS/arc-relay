#!/usr/bin/env bash
# /recall — wrapper for arc-sync memory search.
# Invoked by ~/.claude/commands/recall.md.
#
# Source of truth: scripts/claude-commands/recall.sh in arc-relay.
# Reinstall on a new machine:
#   cp scripts/claude-commands/recall.* ~/.claude/commands/
#   chmod +x ~/.claude/commands/recall.sh
set -euo pipefail
exec arc-sync memory search "$@"
