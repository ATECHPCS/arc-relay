package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// setupCheckDriftMock spins up an httptest server that mimics the relay's
// POST /api/skills/{slug}/check-drift endpoint. The behavior keys off the
// slug:
//   - "drift-skill"       → 200 + drift body
//   - "uptodate-skill"    → 204
//   - "no-upstream-skill" → 409
//   - "missing-skill"     → 404
//   - "broken-skill"      → 502
//   - anything else       → 500 (catches unexpected requests)
func setupCheckDriftMock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		// Path shape: /api/skills/{slug}/check-drift
		const prefix = "/api/skills/"
		const suffix = "/check-drift"
		if len(r.URL.Path) <= len(prefix)+len(suffix) ||
			r.URL.Path[:len(prefix)] != prefix ||
			r.URL.Path[len(r.URL.Path)-len(suffix):] != suffix {
			http.NotFound(w, r)
			return
		}
		slug := r.URL.Path[len(prefix) : len(r.URL.Path)-len(suffix)]

		switch slug {
		case "drift-skill":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outdated": true,
				"drift": map[string]any{
					"severity":           "minor",
					"summary":            "3 commits behind",
					"recommended_action": "run `arc-sync skill push`",
					"commits_ahead":      3,
					"upstream_sha":       "abc123",
					"detected_at":        "2026-04-30T12:00:00Z",
					"relay_version":      "0.1.0",
					"relay_hash":         "ef12",
					"llm_model":          "gpt-4o-mini",
				},
			})
		case "uptodate-skill":
			w.WriteHeader(http.StatusNoContent)
		case "no-upstream-skill":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no upstream configured"})
		case "missing-skill":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "skill not found"})
		case "broken-skill":
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "upstream fetch failed"})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

func TestCheckDrift_Drift(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	drift, err := c.CheckDrift("drift-skill")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift == nil {
		t.Fatal("expected drift block, got nil")
	}
	if drift.Severity != "minor" {
		t.Errorf("Severity = %q, want %q", drift.Severity, "minor")
	}
	if drift.Summary != "3 commits behind" {
		t.Errorf("Summary = %q", drift.Summary)
	}
	if drift.RecommendedAction != "run `arc-sync skill push`" {
		t.Errorf("RecommendedAction = %q", drift.RecommendedAction)
	}
	if drift.CommitsAhead != 3 {
		t.Errorf("CommitsAhead = %d, want 3", drift.CommitsAhead)
	}
	if drift.UpstreamSHA != "abc123" {
		t.Errorf("UpstreamSHA = %q", drift.UpstreamSHA)
	}
	want, _ := time.Parse(time.RFC3339, "2026-04-30T12:00:00Z")
	if !drift.DetectedAt.Equal(want) {
		t.Errorf("DetectedAt = %v, want %v", drift.DetectedAt, want)
	}
	if drift.LLMModel != "gpt-4o-mini" {
		t.Errorf("LLMModel = %q", drift.LLMModel)
	}
}

func TestCheckDrift_UpToDate(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	drift, err := c.CheckDrift("uptodate-skill")
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if drift != nil {
		t.Errorf("expected nil drift on 204, got %+v", drift)
	}
}

func TestCheckDrift_NoUpstream(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.CheckDrift("no-upstream-skill")
	if err == nil {
		t.Fatal("expected error on 409")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusConflict {
		t.Errorf("Status = %d, want %d", httpErr.Status, http.StatusConflict)
	}
}

func TestCheckDrift_NotFound(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.CheckDrift("missing-skill")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want %d", httpErr.Status, http.StatusNotFound)
	}
}

func TestCheckDrift_UpstreamFetchFailed(t *testing.T) {
	ts := setupCheckDriftMock(t)
	defer ts.Close()

	c := NewClient(ts.URL, "key")
	_, err := c.CheckDrift("broken-skill")
	if err == nil {
		t.Fatal("expected error on 502")
	}
	var httpErr *SkillHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *SkillHTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusBadGateway {
		t.Errorf("Status = %d, want %d", httpErr.Status, http.StatusBadGateway)
	}
}

// TestSkill_OutdatedJSON verifies the JSON shape: when the relay sends
// `outdated:1` + `drift:{...}` on a list/detail row, our Skill struct picks
// both up. This guards against silent wire-shape regressions.
func TestSkill_OutdatedJSON(t *testing.T) {
	raw := []byte(`{
		"id":"id1","slug":"foo","display_name":"Foo","description":"",
		"visibility":"public","latest_version":"1.0.0",
		"created_at":"2026-04-30T00:00:00Z","updated_at":"2026-04-30T00:00:00Z",
		"outdated":1,
		"drift":{"severity":"security","summary":"CVE-2024-12345","recommended_action":"upgrade now","commits_ahead":7,"upstream_sha":"deadbeef","detected_at":"2026-04-30T12:00:00Z","relay_version":"0.2.0","relay_hash":"abcd","llm_model":"gpt-4o-mini"}
	}`)
	var s Skill
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Outdated != 1 {
		t.Errorf("Outdated = %d, want 1", s.Outdated)
	}
	if s.Drift == nil {
		t.Fatal("expected non-nil Drift block")
	}
	if s.Drift.Severity != "security" {
		t.Errorf("Drift.Severity = %q, want %q", s.Drift.Severity, "security")
	}
	if s.Drift.CommitsAhead != 7 {
		t.Errorf("Drift.CommitsAhead = %d, want 7", s.Drift.CommitsAhead)
	}
}
