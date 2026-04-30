package checker

// cron_test.go is an integration test for the checker Service. It uses a real
// SQLite-backed SkillStore (via testutil.OpenTestFileDB so cross-goroutine
// connections work even though we don't spin up a goroutine here) plus a real
// fixture git repo (via makeTestRepo from git_test.go).
//
// Each subtest covers one of the four Detect outcomes end-to-end and asserts
// the resulting skill_upstreams row state.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// newCheckerForTest wires a Service against a fresh test DB + a checker cfg
// pointing at a tempdir cache. Returns the service, the SkillStore (so tests
// can seed skills + assert state), and the cache directory.
func newCheckerForTest(t *testing.T) (*Service, *store.SkillStore, string) {
	t.Helper()
	db := testutil.OpenTestFileDB(t)
	skillStore := store.NewSkillStore(db)
	cacheDir := filepath.Join(t.TempDir(), "upstream-cache")
	cfg := config.SkillsCheckerConfig{
		Enabled:          true,
		UpstreamCacheDir: cacheDir,
		LLMDiffMaxBytes:  4096,
	}
	svc := NewService(skillStore, nil, cfg)
	return svc, skillStore, cacheDir
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

// TestCheckOne_Drift exercises the full drift path: baseline registered, real
// content change upstream, RunOnce dispatches to checkOne, which writes a
// drift report and flips skills.outdated to 1.
func TestCheckOne_Drift(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	svc, skillStore, cacheDir := newCheckerForTest(t)

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

	// Verify skill_upstreams row got a drift report.
	got, err := skillStore.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got == nil {
		t.Fatalf("upstream row missing")
	}
	if got.DriftSeverity == nil || *got.DriftSeverity != "unknown" {
		t.Fatalf("DriftSeverity = %v, want %q", deref(got.DriftSeverity), "unknown")
	}
	if got.DriftSummary == nil || *got.DriftSummary != "drift detected" {
		t.Fatalf("DriftSummary = %v, want %q", deref(got.DriftSummary), "drift detected")
	}
	if got.DriftRecommendedAction == nil || *got.DriftRecommendedAction != "review changes" {
		t.Fatalf("DriftRecommendedAction = %v, want %q", deref(got.DriftRecommendedAction), "review changes")
	}
	if got.DriftLLMModel == nil || *got.DriftLLMModel != "" {
		t.Fatalf("DriftLLMModel = %v, want empty", deref(got.DriftLLMModel))
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

// TestCheckOne_NoMovement exercises the cheapest path: upstream HEAD hasn't
// moved since lastSeenSHA, so we just bump last_checked_at and leave drift
// state alone.
func TestCheckOne_NoMovement(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	svc, skillStore, cacheDir := newCheckerForTest(t)
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

	svc, skillStore, cacheDir := newCheckerForTest(t)
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
