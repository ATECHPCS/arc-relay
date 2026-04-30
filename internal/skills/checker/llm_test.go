package checker

// llm_test.go covers Classify across all four runtime paths:
//
//   1. LLM happy path: valid JSON envelope → DriftReport severity flows from
//      the model, LLMModel reports the configured model.
//   2. LLM invalid JSON: bad shape / unknown severity → fallback wiring with
//      severity="unknown" and LLMModel="".
//   3. LLM error 500: HTTP failure swallowed; same fallback wiring as #2.
//   4. LLM unavailable (no API key): Classify must NOT make an HTTP call.
//
// Plus a focused fallback-content assertion: the canned summary contains the
// commit count and file count, and the recommended action is the canned
// string.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// fixtureSkill returns a skill with realistic-looking metadata for prompt
// content checks.
func fixtureSkill() *store.Skill {
	return &store.Skill{
		ID:            "sk_test_1",
		Slug:          "demo-skill",
		DisplayName:   "Demo Skill",
		Description:   "A skill used in checker tests",
		LatestVersion: "0.1.0",
	}
}

// fixtureDetection returns a Drift Detection with non-trivial commit and
// changed-file counts so the fallback summary has real numbers to render.
func fixtureDetection() *Detection {
	return &Detection{
		Result:       ResultDrift,
		NewSHA:       "deadbeefcafefeed",
		NewHash:      "subhash:abcdef0123456789",
		CommitsAhead: 3,
		ChangedFiles: []string{"SKILL.md", "scripts/run.sh", "README.md"},
		DiffSummary: " SKILL.md     | 4 +-\n" +
			" scripts/run.sh | 12 ++++++------\n" +
			" README.md    | 2 +-\n",
	}
}

// stubChatServer replies to /chat/completions with the given content as the
// assistant message, returning an OpenAI-shaped envelope. captureCount
// pointer is bumped on every request hit.
func stubChatServer(t *testing.T, content string, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") && r.URL.Path != "/" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Drain body so the client doesn't see EOF mid-write.
		_, _ = io.ReadAll(r.Body)
		envelope := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 4},
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
}

func TestClassify_LLMHappyPath(t *testing.T) {
	hits := 0
	body, _ := json.Marshal(map[string]string{
		"severity":           "minor",
		"summary":            "Documentation tweaks only.",
		"recommended_action": "Pull at your leisure; no behavior change.",
	})
	srv := stubChatServer(t, string(body), &hits)
	defer srv.Close()

	c := llm.NewClient("test-key", "gpt-4o-mini")
	c.SetBaseURLForTest(srv.URL)

	det := fixtureDetection()
	skill := fixtureSkill()

	report, err := Classify(context.Background(), c, skill, det, "subhash:relay123", "0.1.0")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 LLM call, got %d", hits)
	}
	if report.Severity != "minor" {
		t.Errorf("Severity = %q, want %q", report.Severity, "minor")
	}
	if report.Summary != "Documentation tweaks only." {
		t.Errorf("Summary = %q, want canned LLM text", report.Summary)
	}
	if report.RecommendedAction != "Pull at your leisure; no behavior change." {
		t.Errorf("RecommendedAction = %q, want LLM text", report.RecommendedAction)
	}
	if report.LLMModel != "gpt-4o-mini" {
		t.Errorf("LLMModel = %q, want gpt-4o-mini", report.LLMModel)
	}
	if report.UpstreamSHA != det.NewSHA {
		t.Errorf("UpstreamSHA = %q, want %q", report.UpstreamSHA, det.NewSHA)
	}
	if report.UpstreamHash != det.NewHash {
		t.Errorf("UpstreamHash = %q, want %q", report.UpstreamHash, det.NewHash)
	}
	if report.RelayHash != "subhash:relay123" {
		t.Errorf("RelayHash = %q, want subhash:relay123", report.RelayHash)
	}
	if report.RelayVersion != "0.1.0" {
		t.Errorf("RelayVersion = %q, want 0.1.0", report.RelayVersion)
	}
	if report.CommitsAhead != det.CommitsAhead {
		t.Errorf("CommitsAhead = %d, want %d", report.CommitsAhead, det.CommitsAhead)
	}
	if report.DetectedAt.IsZero() {
		t.Error("DetectedAt should be set to time.Now().UTC(), got zero")
	}
}

func TestClassify_LLMInvalidJSON(t *testing.T) {
	hits := 0
	// Model returns prose instead of JSON — parse will fail → fallback.
	srv := stubChatServer(t, "I think this is a minor change but I'm not sure.", &hits)
	defer srv.Close()

	c := llm.NewClient("test-key", "gpt-4o-mini")
	c.SetBaseURLForTest(srv.URL)

	det := fixtureDetection()
	report, err := Classify(context.Background(), c, fixtureSkill(), det, "subhash:relay", "0.1.0")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected 1 LLM call, got %d", hits)
	}
	if report.Severity != "unknown" {
		t.Errorf("Severity = %q, want unknown (fallback)", report.Severity)
	}
	if report.LLMModel != "" {
		t.Errorf("LLMModel = %q, want empty (fallback)", report.LLMModel)
	}
	if report.RecommendedAction != fallbackRecommendedAction {
		t.Errorf("RecommendedAction = %q, want canned fallback", report.RecommendedAction)
	}
}

func TestClassify_LLMBadSeverity(t *testing.T) {
	// Valid JSON envelope but severity is outside the rubric → fallback.
	body, _ := json.Marshal(map[string]string{
		"severity":           "catastrophic",
		"summary":            "It's the end of the world.",
		"recommended_action": "Panic.",
	})
	srv := stubChatServer(t, string(body), nil)
	defer srv.Close()

	c := llm.NewClient("test-key", "gpt-4o-mini")
	c.SetBaseURLForTest(srv.URL)

	det := fixtureDetection()
	report, err := Classify(context.Background(), c, fixtureSkill(), det, "", "")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if report.Severity != "unknown" {
		t.Errorf("Severity = %q, want unknown (fallback rejected enum)", report.Severity)
	}
	if report.LLMModel != "" {
		t.Errorf("LLMModel = %q, want empty (fallback)", report.LLMModel)
	}
}

func TestClassify_LLMHTTPError(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"server","message":"upstream broke"}}`))
	}))
	defer srv.Close()

	c := llm.NewClient("test-key", "gpt-4o-mini")
	c.SetBaseURLForTest(srv.URL)

	det := fixtureDetection()
	report, err := Classify(context.Background(), c, fixtureSkill(), det, "h", "v")
	if err != nil {
		t.Fatalf("Classify must not return an error on LLM 500: %v", err)
	}
	if hits != 1 {
		t.Errorf("expected 1 LLM call, got %d", hits)
	}
	if report.Severity != "unknown" {
		t.Errorf("Severity = %q, want unknown", report.Severity)
	}
	if report.LLMModel != "" {
		t.Errorf("LLMModel = %q, want empty", report.LLMModel)
	}
}

func TestClassify_LLMUnavailable(t *testing.T) {
	// Server fails the test if hit. Classify must skip the network entirely.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("LLM HTTP must not be invoked when client.Available()==false (path=%s)", r.URL.Path)
	}))
	defer srv.Close()

	// Empty key → Available() == false.
	c := llm.NewClient("", "gpt-4o-mini")
	c.SetBaseURLForTest(srv.URL)
	if c.Available() {
		t.Fatalf("test setup wrong: Available() must be false with empty key")
	}

	det := fixtureDetection()
	report, err := Classify(context.Background(), c, fixtureSkill(), det, "h", "v")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if report.Severity != "unknown" {
		t.Errorf("Severity = %q, want unknown", report.Severity)
	}
	if report.LLMModel != "" {
		t.Errorf("LLMModel = %q, want empty (no model used)", report.LLMModel)
	}
}

func TestClassify_NilLLMClient(t *testing.T) {
	// Belt-and-suspenders: Classify must tolerate a nil llm.Client and treat
	// it the same as an unavailable one.
	det := fixtureDetection()
	report, err := Classify(context.Background(), nil, fixtureSkill(), det, "h", "v")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if report.Severity != "unknown" {
		t.Errorf("Severity = %q, want unknown", report.Severity)
	}
	if report.LLMModel != "" {
		t.Errorf("LLMModel = %q, want empty", report.LLMModel)
	}
}

func TestClassify_FallbackSummaryContainsCounts(t *testing.T) {
	// Fallback path should produce a Summary that includes both the commit
	// count and the changed-file count so an operator can eyeball the report
	// without an LLM.
	det := fixtureDetection() // 3 commits, 3 changed files
	report, err := Classify(context.Background(), nil, fixtureSkill(), det, "h", "v")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	wantCommits := fmt.Sprintf("%d", det.CommitsAhead)
	wantFiles := fmt.Sprintf("%d", len(det.ChangedFiles))
	if !strings.Contains(report.Summary, wantCommits) {
		t.Errorf("Summary %q must mention commit count %s", report.Summary, wantCommits)
	}
	if !strings.Contains(report.Summary, wantFiles) {
		t.Errorf("Summary %q must mention changed-file count %s", report.Summary, wantFiles)
	}
	if report.RecommendedAction != fallbackRecommendedAction {
		t.Errorf("RecommendedAction = %q, want canned fallback", report.RecommendedAction)
	}
}
