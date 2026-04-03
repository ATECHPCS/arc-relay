# arc-sync CLI Changes for Git Build Support

## 1. Increase HTTP timeout for server actions

**File:** `internal/wrangler/client.go`

The global 10s timeout kills in-progress Docker builds. Use a longer timeout for `StartServer`/`StopServer` calls (or per-request context).

**Option A — Increase default timeout:**
```go
HTTPClient: &http.Client{
    Timeout: 5 * time.Minute,
},
```

**Option B (better) — Per-request timeout in `serverAction`:**
```go
func (c *Client) serverAction(serverID, action string) error {
    url := fmt.Sprintf("%s/api/servers/%s/%s", c.BaseURL, serverID, action)

    // Server start may trigger a Docker build which can take minutes
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
    defer cancel()

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
    if err != nil {
        return fmt.Errorf("creating request: %w", err)
    }
    req.Header.Set("Authorization", "Bearer "+c.APIKey)

    // Use a client without the default timeout since we have a per-request context
    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return fmt.Errorf("connecting to wrangler: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
        return nil
    }

    body, _ := io.ReadAll(resp.Body)
    return handleErrorResponse(resp, body, fmt.Sprintf("server %q", serverID))
}
```

## 2. Add `GitRef` field to `StdioBuildConfig`

**File:** `internal/wrangler/types.go`

```go
type StdioBuildConfig struct {
    Runtime string `json:"runtime"`           // "python" or "node"
    Package string `json:"package"`           // pip/npm package name
    Version string `json:"version,omitempty"` // package version (empty = latest)
    GitURL  string `json:"git_url,omitempty"` // alternative: build from git repo
    GitRef  string `json:"git_ref,omitempty"` // branch, tag, or commit hash
}
```

## 3. Add `--git-url` and `--git-ref` flags to `server add`

**File:** `cmd/arc-sync/main.go`

In the help text for `server add`, add:
```
  arc-sync server add <name> --type stdio --build python --git-url https://github.com/user/repo
  arc-sync server add <name> --type stdio --build python --git-url https://github.com/user/repo --git-ref v1.0.0
```

In the options section:
```
  --git-url <url>          Git repository URL (alternative to --package, HTTPS only)
  --git-ref <ref>          Branch, tag, or commit (optional, default branch if omitted)
```

In `buildStdioConfig()`, parse the new flags and populate the config. When `--git-url` is provided without `--package`, the wrangler will clone the repo and use its own Dockerfile if one exists.

## 4. Note on the Slack server

The slack server failed because the package name was wrong. The correct name is:
```
arc-sync server add slack \
    --type stdio \
    --build node \
    --package @modelcontextprotocol/server-slack \
    --display-name "Slack" \
    --env SLACK_BOT_TOKEN=xoxb-... \
    --env SLACK_TEAM_ID=T... \
    --start
```

## Server-side fix already deployed

The wrangler now uses a detached context (10min timeout) for the `/api/servers/{id}/start` endpoint, so even if the CLI times out, the build continues to completion on the server. The CLI timeout increase is still recommended for a better UX (so the CLI waits for the result and prints success/failure instead of a timeout warning).
