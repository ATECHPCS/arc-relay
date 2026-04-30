package checker

// cron_test.go is an integration test for the checker Service. It uses a real
// SQLite-backed SkillStore (via testutil.OpenTestFileDB so cross-goroutine
// connections work even though we don't spin up a goroutine here) plus a real
// fixture git repo (via makeTestRepo from git_test.go).
//
// Each subtest covers one of the four Detect outcomes end-to-end and asserts
// the resulting skill_upstreams row state. The Drift path additionally
// exercises the LLM classifier wiring: one subtest stubs an httptest server
// returning a known JSON envelope (assert severity flows through), and one
// subtest uses an empty-key llm.Client (assert "unknown" fallback).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// newCheckerForTest wires a Service against a fresh test DB + a checker cfg
// pointing at a tempdir cache. Pass a non-nil llmClient to exercise the
// classifier path; nil tolerates the same nil-Client path Classify already
// handles. Returns the service, the SkillStore (so tests can seed skills +
// assert state), and the cache directory.
func newCheckerForTest(t *testing.T, llmClient *llm.Client) (*Service, *store.SkillStore, string) {
	t.Helper()
	db := testutil.OpenTestFileDB(t)
	skillStore := store.NewSkillStore(db)
	cacheDir := filepath.Join(t.TempDir(), "upstream-cache")
	cfg := config.SkillsCheckerConfig{
		Enabled:          true,
		UpstreamCacheDir: cacheDir,
		LLMDiffMaxBytes:  4096,
	}
	// skillsSvc is left nil — the Drift tests don't need a relay-side hash
	// (the skill rows seeded here have no LatestVersion + no archive on
	// disk), and relayHashForSkill short-circuits to "" when LatestVersion
	// is empty regardless of skillsSvc.
	svc := NewService(skillStore, nil, llmClient, cfg)
	return svc, skillStore, cacheDir
}

// stubLLMServer mints a chat-completions httptest server that returns the
// given JSON envelope (already-encoded message content) as the assistant
// reply. Used by TestCheckOne_Drift.
func stubLLMServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// seedSkillAndUpstream inserts a skill row + skill_upstreams row pointing at
// the given fileURL/subpath, with the given lastSeenSHA/lastSeenHash baseline.
// Returns the created skill ID.
func seedSkillAndUpstream(t *testing.T, st *store.SkillStore, slug, gitURL, subpath, lastSeenSHA, lastSeenHash string) string {
	t.Helper()
	sk := &store.Skill{
		Slug:        slug,
		DisplayName: slug,
		Description: "fixture",
	}
	if err := st.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	u := &store.SkillUpstream{
		SkillID:    sk.ID,
		GitURL:     gitURL,
		GitSubpath: subpath,
		GitRef:     "origin/main",
	}
	if lastSeenSHA != "" {
		u.LastSeenSHA = &lastSeenSHA
	}
	if lastSeenHash != "" {
		u.LastSeenHash = &lastSeenHash
	}
	if err := st.UpsertUpstream(u); err != nil {
		t.Fatalf("UpsertUpstream: %v", err)
	}
	return sk.ID
}

// hashSubpathAt is duplicated from detect_test.go via the same package — but
// detect_test.go's helper already lives in this package, so we reuse it.

// TestCheckOne_Drift exercises the full drift path with a stubbed LLM:
// baseline registered, real content change upstream, RunOnce dispatches to
// checkOne, which calls Classify (against our httptest server) and persists
// the resulting DriftReport. Severity, summary, action, and llm_model must
// all reflect the LLM's response — not the placeholder values the prior
// implementation wrote.
func TestCheckOne_Drift(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	envelope, _ := json.Marshal(map[string]string{
		"severity":           "minor",
		"summary":            "Two files updated; non-functional changes only.",
		"recommended_action": "Pull at next maintenance window.",
	})
	srv := stubLLMServer(t, string(envelope))
	defer srv.Close()
	llmClient := llm.NewClient("test-key", "gpt-4o-mini")
	llmClient.SetBaseURLForTest(srv.URL)

	svc, skillStore, cacheDir := newCheckerForTest(t, llmClient)

	// Pre-clone so we can resolve the baseline SHA + subpath hash to seed the
	// skill_upstreams row in steady state. Without a baseline Detect treats
	// the first check as drift, which is correct but doesn't exercise the
	// happy-path "we had a baseline, then it changed" path we want to test.
	baselineSHA, baselineHash := primeCacheBaseline(t, src, cacheDir, "skills/foo")

	skillID := seedSkillAndUpstream(t, skillStore, "drift-skill",
		fileURL(src), "skills/foo", baselineSHA, baselineHash)

	// Real content drift upstream.
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v2\nadded\n"}, "skills/foo: v2")

	svc.RunOnce(context.Background())

	// Verify skill_upstreams row got a drift report from the LLM.
	got, err := skillStore.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got == nil {
		t.Fatalf("upstream row missing")
	}
	if got.DriftSeverity == nil || *got.DriftSeverity != "minor" {
		t.Fatalf("DriftSeverity = %v, want %q (from LLM)", deref(got.DriftSeverity), "minor")
	}
	if got.DriftSummary == nil || *got.DriftSummary != "Two files updated; non-functional changes only." {
		t.Fatalf("DriftSummary = %v, want LLM text", deref(got.DriftSummary))
	}
	if got.DriftRecommendedAction == nil || *got.DriftRecommendedAction != "Pull at next maintenance window." {
		t.Fatalf("DriftRecommendedAction = %v, want LLM text", deref(got.DriftRecommendedAction))
	}
	if got.DriftLLMModel == nil || *got.DriftLLMModel != "gpt-4o-mini" {
		t.Fatalf("DriftLLMModel = %v, want %q", deref(got.DriftLLMModel), "gpt-4o-mini")
	}
	if got.DriftCommitsAhead == nil || *got.DriftCommitsAhead < 1 {
		t.Fatalf("DriftCommitsAhead = %v, want >= 1", got.DriftCommitsAhead)
	}

	// Verify skill.outdated flipped to 1.
	sk, err := skillStore.GetSkill(skillID)
	if err != nil {
		t.Fatalf("GetSkill: %v", err)
	}
	if sk.Outdated != 1 {
		t.Fatalf("skill.outdated = %d, want 1", sk.Outdated)
	}
}

// TestCheckOne_Drift_NoLLMKey exercises the cron drift path when the LLM
// client is configured but has no API key (Available()==false). Classify
// must skip the network and return the deterministic fallback triple, which
// the cron then persists. Severity must be "unknown" + LLMModel "".
func TestCheckOne_Drift_NoLLMKey(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	// Empty-key client → Available()==false. No httptest server: any HTTP
	// call would fail loudly, which is exactly what we want to detect.
	llmClient := llm.NewClient("", "gpt-4o-mini")
	if llmClient.Available() {
		t.Fatalf("test setup: empty-key client must be unavailable")
	}

	svc, skillStore, cacheDir := newCheckerForTest(t, llmClient)
	baselineSHA, baselineHash := primeCacheBaseline(t, src, cacheDir, "skills/foo")
	skillID := seedSkillAndUpstream(t, skillStore, "drift-nokey-skill",
		fileURL(src), "skills/foo", baselineSHA, baselineHash)

	// Real content drift.
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v2\n"}, "skills/foo: v2")

	svc.RunOnce(context.Background())

	got, err := skillStore.GetUpstream(skillID)
	if err != nil || got == nil {
		t.Fatalf("GetUpstream: u=%v err=%v", got, err)
	}
	if got.DriftSeverity == nil || *got.DriftSeverity != "unknown" {
		t.Fatalf("DriftSeverity = %v, want %q (fallback)", deref(got.DriftSeverity), "unknown")
	}
	if got.DriftLLMModel == nil || *got.DriftLLMModel != "" {
		t.Fatalf("DriftLLMModel = %v, want empty (fallback)", deref(got.DriftLLMModel))
	}
	if got.DriftRecommendedAction == nil || *got.DriftRecommendedAction != fallbackRecommendedAction {
		t.Fatalf("DriftRecommendedAction = %v, want canned fallback", deref(got.DriftRecommendedAction))
	}
}

// TestCheckOne_NoMovement exercises the cheapest path: upstream HEAD hasn't
// moved since lastSeenSHA, so we just bump last_checked_at and leave drift
// state alone.
func TestCheckOne_NoMovement(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	svc, skillStore, cacheDir := newCheckerForTest(t, nil)
	baselineSHA, baselineHash := primeCacheBaseline(t, src, cacheDir, "skills/foo")

	skillID := seedSkillAndUpstream(t, skillStore, "still-skill",
		fileURL(src), "skills/foo", baselineSHA, baselineHash)

	svc.RunOnce(context.Background())

	got, err := skillStore.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got.LastCheckedAt == nil {
		t.Fatalf("LastCheckedAt nil after RunOnce — should have been bumped")
	}
	if got.DriftSeverity != nil {
		t.Fatalf("DriftSeverity = %q, want nil (no drift)", *got.DriftSeverity)
	}
	if got.LastSeenSHA == nil || *got.LastSeenSHA != baselineSHA {
		t.Fatalf("LastSeenSHA = %v, want %q", deref(got.LastSeenSHA), baselineSHA)
	}
	if got.LastSeenHash == nil || *got.LastSeenHash != baselineHash {
		t.Fatalf("LastSeenHash = %v, want %q", deref(got.LastSeenHash), baselineHash)
	}

	sk, err := skillStore.GetSkill(skillID)
	if err != nil {
		t.Fatalf("GetSkill: %v", err)
	}
	if sk.Outdated != 0 {
		t.Fatalf("skill.outdated = %d, want 0", sk.Outdated)
	}
}

// TestCheckOne_NoPathTouch exercises HEAD moved but the subpath wasn't
// touched: last_seen_sha advances to the new SHA; last_seen_hash unchanged.
func TestCheckOne_NoPathTouch(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	svc, skillStore, cacheDir := newCheckerForTest(t, nil)
	baselineSHA, baselineHash := primeCacheBaseline(t, src, cacheDir, "skills/foo")

	skillID := seedSkillAndUpstream(t, skillStore, "movehead-skill",
		fileURL(src), "skills/foo", baselineSHA, baselineHash)

	// Top-level commit that doesn't touch skills/foo.
	addCommit(t, src, map[string]string{"top.txt": "x\n"}, "top-level only")

	svc.RunOnce(context.Background())

	got, err := skillStore.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got.DriftSeverity != nil {
		t.Fatalf("DriftSeverity = %q, want nil", *got.DriftSeverity)
	}
	if got.LastSeenSHA == nil || *got.LastSeenSHA == baselineSHA {
		t.Fatalf("LastSeenSHA should have advanced past baseline; got %q baseline %q",
			deref(got.LastSeenSHA), baselineSHA)
	}
	if got.LastSeenHash == nil || *got.LastSeenHash != baselineHash {
		t.Fatalf("LastSeenHash should be unchanged: got %q want %q",
			deref(got.LastSeenHash), baselineHash)
	}
}

// primeCacheBaseline clones src into cacheDir/<auto>, resolves the current
// origin/main SHA, and computes the subpath hash. Returns (sha, hash). The
// auto-generated subdir matches Service.cacheDirFor's layout so when the
// service later calls EnsureCache against this same upstream URL it lands on
// the same on-disk cache.
func primeCacheBaseline(t *testing.T, src, cacheRoot, subpath string) (string, string) {
	t.Helper()
	// Use a temp Service strictly to compute the deterministic cacheDirFor
	// path; everything else is real-world behavior.
	svc := &Service{cfg: config.SkillsCheckerConfig{UpstreamCacheDir: cacheRoot}}
	dir := svc.cacheDirFor(&store.SkillUpstream{
		GitURL:     fileURL(src),
		GitSubpath: subpath,
	})
	if err := EnsureCache(context.Background(), dir, fileURL(src)); err != nil {
		t.Fatalf("prime: EnsureCache: %v", err)
	}
	sha, err := ResolveSHA(context.Background(), dir, "origin/main")
	if err != nil {
		t.Fatalf("prime: ResolveSHA: %v", err)
	}
	hash := hashSubpathAt(t, dir, sha, subpath)
	return sha, hash
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
