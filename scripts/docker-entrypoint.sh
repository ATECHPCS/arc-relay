#!/bin/sh
# arc-relay container entrypoint.
#
# The image is built with an unprivileged 'arcrelay' user (UID/GID 65532).
# The server binary itself never needs root, but two boot-time tasks do:
#
#  1. The /data volume is created by Docker on first start with root ownership;
#     SQLite needs to write to it, so it must be chowned to the unprivileged
#     user. Subsequent restarts find ownership already correct and skip the
#     recursive walk.
#
#  2. Arc Relay manages MCP server containers via /var/run/docker.sock. The
#     socket is owned by root:<docker-gid-on-host> and the GID varies between
#     hosts (commonly 999, 996, etc.). We detect the socket's GID at runtime
#     and add 'arcrelay' to a matching supplementary group so Docker API calls
#     work without granting broad privileges.
#
# After both fixups, we drop privileges via su-exec and exec the binary as
# arcrelay. The PID 1 root window is contained to this short script; the
# server process itself runs unprivileged for the entire request lifetime.

set -eu

DATA_DIR="${ARC_RELAY_DATA_DIR:-/data}"
TARGET_USER="arcrelay"
DOCKER_SOCK="/var/run/docker.sock"

if [ "$(id -u)" != "0" ]; then
    # Already unprivileged (e.g., docker run --user); just exec.
    exec /usr/local/bin/arc-relay "$@"
fi

# --- 1. Fix ownership on the data volume ----------------------------------
# Avoid recursive chown on every restart: if the top-level dir is already
# owned by arcrelay we trust the rest of the tree and skip. Operators can
# force a re-chown by `chown root:root /data` before restart if a previous
# upgrade left mixed ownership.
if [ -d "$DATA_DIR" ]; then
    cur_owner=$(stat -c '%U' "$DATA_DIR" 2>/dev/null || echo "?")
    if [ "$cur_owner" != "$TARGET_USER" ]; then
        echo "arc-relay-entrypoint: chowning $DATA_DIR to $TARGET_USER (was: $cur_owner)" >&2
        chown -R "$TARGET_USER:$TARGET_USER" "$DATA_DIR"
    fi
fi

# --- 2. Match arcrelay to the docker.sock GID -----------------------------
# Only relevant if the socket is mounted into the container. Without this
# the binary's docker.NewClient calls fail with "permission denied" on every
# StartServer invocation.
if [ -S "$DOCKER_SOCK" ]; then
    sock_gid=$(stat -c '%g' "$DOCKER_SOCK")
    # Skip if it's GID 0 (root); arcrelay isn't going to be in the root group.
    if [ "$sock_gid" != "0" ]; then
        sock_group=$(getent group "$sock_gid" | cut -d: -f1 || true)
        if [ -z "$sock_group" ]; then
            # No existing group with that GID — synthesize one. Name doesn't
            # matter to the kernel; only the GID does.
            sock_group="docker-host"
            addgroup -g "$sock_gid" -S "$sock_group"
        fi
        # addgroup is idempotent for "user is already in group"; treat as best-effort.
        if ! id -nG "$TARGET_USER" | tr ' ' '\n' | grep -qx "$sock_group"; then
            addgroup "$TARGET_USER" "$sock_group"
            echo "arc-relay-entrypoint: added $TARGET_USER to $sock_group (gid=$sock_gid)" >&2
        fi
    fi
fi

# --- 3. Drop privileges and exec the binary -------------------------------
exec su-exec "$TARGET_USER" /usr/local/bin/arc-relay "$@"
