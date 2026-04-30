package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Skill mirrors the relay's store.Skill JSON shape. Defined here (vs imported
// from internal/store) so arc-sync stays a pure-Go binary with no CGO/sqlite
// linkage. Wire shape kept in sync by hand.
type Skill struct {
	ID            string     `json:"id"`
	Slug          string     `json:"slug"`
	DisplayName   string     `json:"display_name"`
	Description   string     `json:"description"`
	Visibility    string     `json:"visibility"`
	LatestVersion string     `json:"latest_version,omitempty"`
	YankedAt      *time.Time `json:"yanked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// SkillVersion mirrors the relay's store.SkillVersion JSON shape.
type SkillVersion struct {
	SkillID       string          `json:"skill_id"`
	Version       string          `json:"version"`
	ArchivePath   string          `json:"archive_path"`
	ArchiveSize   int64           `json:"archive_size"`
	ArchiveSHA256 string          `json:"archive_sha256"`
	Manifest      json.RawMessage `json:"manifest"`
	YankedAt      *time.Time      `json:"yanked_at,omitempty"`
	UploadedAt    time.Time       `json:"uploaded_at"`
}

// AssignedSkill is the row shape from GET /api/skills/assigned.
type AssignedSkill struct {
	Skill         *Skill  `json:"skill"`
	PinnedVersion *string `json:"pinned_version,omitempty"`
}

// SkillDetail is the response from GET /api/skills/{slug}.
type SkillDetail struct {
	Skill    *Skill          `json:"skill"`
	Versions []*SkillVersion `json:"versions"`
}

// UploadSkillResult is the response from POST /api/skills/{slug}/versions/{version}.
type UploadSkillResult struct {
	Skill   *Skill        `json:"skill"`
	Version *SkillVersion `json:"version"`
}

// UpstreamMetadata is the JSON shape sent in the `X-Upstream` header on a
// version upload (see Task 4: internal/web/skills_handlers.go's
// upstreamHeaderPayload — the shapes must match).
//
// Sentinel: empty Type AND empty URL means "clear the recorded upstream".
// UploadSkill consults this to decide between `X-Upstream: <json>` and
// `X-Clear-Upstream: true`. Defined in this package (rather than the higher-
// level sync package) so UploadSkill can take it as a parameter without
// creating a cycle — sync imports relay today.
type UpstreamMetadata struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Subpath string `json:"subpath"`
	Ref     string `json:"ref"`
}

// ListSkills calls GET /api/skills. Returns whatever the user can see — admin
// gets the full catalog (incl. yanked); non-admin gets public + assigned.
func (c *Client) ListSkills() ([]*Skill, error) {
	body, err := c.skillGet("/api/skills")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Skills []*Skill `json:"skills"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse skills list: %w", err)
	}
	return resp.Skills, nil
}

// ListAssignedSkills calls GET /api/skills/assigned. Used by `arc-sync skill
// sync` to compute the desired client state.
func (c *Client) ListAssignedSkills() ([]*AssignedSkill, error) {
	body, err := c.skillGet("/api/skills/assigned")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Assigned []*AssignedSkill `json:"assigned"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse assigned skills: %w", err)
	}
	return resp.Assigned, nil
}

// GetSkill calls GET /api/skills/{slug} and returns the metadata + version list.
// Returns nil with no error if the skill doesn't exist or the user can't see it
// (HTTP 404).
func (c *Client) GetSkill(slug string) (*SkillDetail, error) {
	body, err := c.skillGet("/api/skills/" + url.PathEscape(slug))
	if err != nil {
		if e, ok := err.(*skillHTTPError); ok && e.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	var resp SkillDetail
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse skill detail: %w", err)
	}
	return &resp, nil
}

// DownloadSkillVersion fetches the archive bytes for (slug, version). Returns
// the body, the SHA-256 from the X-Skill-SHA256 response header (used by the
// caller to verify integrity post-download), and the size in bytes.
func (c *Client) DownloadSkillVersion(slug, version string) (archive []byte, sha256 string, err error) {
	endpoint := fmt.Sprintf("/api/skills/%s/versions/%s/archive",
		url.PathEscape(slug), url.PathEscape(version))
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("reading archive: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", handleErrorResponse(resp, body, fmt.Sprintf("skill %q@%s", slug, version))
	}
	return body, resp.Header.Get("X-Skill-SHA256"), nil
}

// UploadSkill posts an archive to POST /api/skills/{slug}/versions/{version}.
// Body is the raw .tar.gz bytes. Visibility is one of "public", "restricted",
// or "" (server default = "restricted" on first publish; ignored on
// re-publish).
//
// upstream carries optional upstream-tracking metadata (see Task 6 of the
// skill update checker plan). It maps to the relay's two-header protocol:
//   - upstream == nil: send neither header (no upstream change requested).
//   - upstream != nil with empty Type AND empty URL (the clear sentinel): send
//     `X-Clear-Upstream: true` to disassociate the skill from any prior upstream.
//   - upstream != nil with non-empty URL: marshal to JSON and send as the
//     `X-Upstream` header. Empty Type defaults to "git" server-side.
func (c *Client) UploadSkill(slug, version, visibility string, archive []byte, upstream *UpstreamMetadata) (*UploadSkillResult, error) {
	q := url.Values{}
	if visibility != "" {
		q.Set("visibility", visibility)
	}
	endpoint := fmt.Sprintf("/api/skills/%s/versions/%s",
		url.PathEscape(slug), url.PathEscape(version))
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+endpoint, bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/gzip")
	if upstream != nil {
		// Sentinel: empty Type + empty URL → clear request. Anything else
		// (even a partially-filled struct with just URL) → record/update.
		if upstream.Type == "" && upstream.URL == "" {
			req.Header.Set("X-Clear-Upstream", "true")
		} else {
			payload, err := json.Marshal(upstream)
			if err != nil {
				return nil, fmt.Errorf("marshal upstream metadata: %w", err)
			}
			req.Header.Set("X-Upstream", string(payload))
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, handleErrorResponse(resp, body, fmt.Sprintf("skill %q@%s", slug, version))
	}
	var out UploadSkillResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse upload response: %w", err)
	}
	return &out, nil
}

// AssignSkill grants a user access to a restricted skill.
// POST /api/skills/{slug}/assignments with body {username, version?}.
// Idempotent: re-assigning replaces any prior version pin.
func (c *Client) AssignSkill(slug, username, version string) error {
	body, err := json.Marshal(map[string]string{"username": username, "version": version})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	endpoint := fmt.Sprintf("/api/skills/%s/assignments", url.PathEscape(slug))
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return handleErrorResponse(resp, respBody, fmt.Sprintf("skill %q assign %q", slug, username))
	}
	return nil
}

// UnassignSkill revokes a user's grant.
// DELETE /api/skills/{slug}/assignments/{username}.
func (c *Client) UnassignSkill(slug, username string) error {
	endpoint := fmt.Sprintf("/api/skills/%s/assignments/%s",
		url.PathEscape(slug), url.PathEscape(username))
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return handleErrorResponse(resp, body, fmt.Sprintf("skill %q unassign %q", slug, username))
	}
	return nil
}

// YankSkill calls DELETE /api/skills/{slug}. Yank is the default; pass hard=true
// to truly delete (admin only on the relay either way).
func (c *Client) YankSkill(slug string, hard bool) error {
	endpoint := "/api/skills/" + url.PathEscape(slug)
	if hard {
		endpoint += "?hard=true"
	}
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return handleErrorResponse(resp, body, fmt.Sprintf("skill %q", slug))
	}
	return nil
}

// skillHTTPError lets ListSkills/GetSkill/etc. distinguish 404-not-found from
// network/auth errors at the call site. handleErrorResponse already returns
// useful errors for non-404s; we surface 404 specifically so GetSkill can
// return (nil, nil).
type skillHTTPError struct {
	Status int
	err    error
}

func (e *skillHTTPError) Error() string { return e.err.Error() }
func (e *skillHTTPError) Unwrap() error { return e.err }

// skillGet is the JSON-API GET wrapper used by the read-side skill methods.
func (c *Client) skillGet(endpoint string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		err := handleErrorResponse(resp, body, "skills")
		return nil, &skillHTTPError{Status: resp.StatusCode, err: err}
	}
	return body, nil
}
