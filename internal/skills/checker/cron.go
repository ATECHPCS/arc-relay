package checker

// cron.go drives the daily upstream-check loop and the single-skill paths
// that the on-demand HTTP endpoint (Task 12) will reuse.
//
// Lifecycle:
//
//   - RunCron: ticker-driven loop matching extractor.RunCron's shape.
//   - RunOnce: one full pass over every upstream registered in the store.
//   - checkOne: the per-upstream orchestration — clone-or-fetch, Detect,
//     dispatch on the four-way outcome.
//
// The four Detect outcomes map to two store updates:
//
//   - NoMovement / NoPathTouch / RevertedToSame → UpdateUpstreamCheck. We
//     advance last_checked_at unconditionally (we did check) and bump
//     last_seen_sha to the current upstream SHA when Detect produced one.
//     For NoMovement the SHA is the same we had; for NoPathTouch and
//     RevertedToSame upstream HEAD moved but the subpath is byte-identical,
//     so we re-pin our pointer without re-hashing.
//   - Drift → WriteDriftReport with placeholder severity/summary. Task 11
//     replaces the placeholders with real LLM output.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// RunCron starts the periodic upstream-check loop. Returns when ctx is
// canceled. Mirrors extractor.RunCron's shape (ticker + select + ctx.Done).
func (s *Service) RunCron(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("skill checker cron loop started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			slog.Info("skill checker cron loop stopped")
			return
		case <-t.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce iterates every registered upstream and runs checkOne against each.
// Errors on a single upstream are logged and counted but never abort the
// cycle — one bad repo shouldn't block the rest.
func (s *Service) RunOnce(ctx context.Context) {
	start := time.Now()
	upstreams, err := s.skills.ListUpstreams()
	if err != nil {
		slog.Error("checker cycle: list upstreams failed", "err", err)
		return
	}
	if len(upstreams) == 0 {
		slog.Debug("checker cycle: no upstreams")
		return
	}

	var ok, fail int
	for _, u := range upstreams {
		// Per-skill timeout: clone+fetch+log+archive can be slow on big
		// monorepos. 4× the configured clone timeout gives all three calls
		// (clone, log, archive) breathing room without letting one bad repo
		// stall the entire cycle.
		callTimeout := s.cfg.GitCloneTimeout * 4
		if callTimeout <= 0 {
			callTimeout = 4 * time.Minute
		}
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		err := s.checkOne(callCtx, u)
		cancel()
		if err != nil {
			slog.Error("checker: checkOne failed",
				"skill_id", u.SkillID, "git_url", u.GitURL, "err", err)
			fail++
			continue
		}
		ok++
	}

	slog.Info("checker cycle complete",
		"upstreams", len(upstreams),
		"ok", ok,
		"fail", fail,
		"ms", time.Since(start).Milliseconds())
}

// checkOne performs one full upstream check for a single skill. On Detect
// errors checksTotal{result="error"} is incremented and the error is
// returned to the caller; on success the appropriate store mutation is
// applied and checksTotal is incremented with the matching result label.
func (s *Service) checkOne(ctx context.Context, u *store.SkillUpstream) error {
	if u == nil {
		return fmt.Errorf("checkOne: upstream is nil")
	}
	timer := prometheusTimer()
	defer timer()

	cacheDir := s.cacheDirFor(u)
	if err := EnsureCache(ctx, cacheDir, u.GitURL); err != nil {
		checksTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("ensure cache: %w", err)
	}

	ref := &UpstreamRef{
		GitSubpath: u.GitSubpath,
		GitRef:     u.GitRef,
	}
	lastSeenSHA := strDeref(u.LastSeenSHA)
	lastSeenHash := strDeref(u.LastSeenHash)

	det, err := Detect(ctx, ref, lastSeenSHA, lastSeenHash, cacheDir, s.cfg.LLMDiffMaxBytes)
	if err != nil {
		checksTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("detect: %w", err)
	}

	now := time.Now().UTC()
	switch det.Result {
	case ResultNoMovement, ResultNoPathTouch, ResultRevertedToSame:
		// All three "no drift" outcomes share the same persistence path:
		// advance last_checked_at + last_seen_sha to the upstream SHA we
		// just resolved; preserve last_seen_hash unchanged (Detect doesn't
		// recompute it for NoMovement/NoPathTouch and RevertedToSame
		// definitionally matches).
		if err := s.skills.UpdateUpstreamCheck(u.SkillID, det.NewSHA, lastSeenHash, now); err != nil {
			checksTotal.WithLabelValues("error").Inc()
			return fmt.Errorf("update upstream check: %w", err)
		}
		checksTotal.WithLabelValues(string(det.Result)).Inc()
		return nil

	case ResultDrift:
		// Placeholder severity + summary. Task 11 replaces these with real
		// LLM output by passing det.DiffSummary + det.ChangedFiles into the
		// classifier. LLMModel="" signals "no model used" to the UI.
		report := &store.DriftReport{
			RelayVersion:      "",
			RelayHash:         lastSeenHash,
			UpstreamSHA:       det.NewSHA,
			UpstreamHash:      det.NewHash,
			CommitsAhead:      det.CommitsAhead,
			Severity:          "unknown",
			Summary:           "drift detected",
			RecommendedAction: "review changes",
			LLMModel:          "",
			DetectedAt:        now,
		}
		if err := s.skills.WriteDriftReport(u.SkillID, report); err != nil {
			checksTotal.WithLabelValues("error").Inc()
			return fmt.Errorf("write drift report: %w", err)
		}
		checksTotal.WithLabelValues("drift").Inc()
		return nil

	default:
		checksTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("checkOne: unknown Detect result %q", det.Result)
	}
}

// cacheDirFor returns the per-upstream cache directory under the configured
// UpstreamCacheDir. We hash (gitURL + subpath) so two skills that share a
// monorepo can have independent caches without colliding, and so a sanitised
// directory name doesn't depend on URL escaping rules.
//
// We also include a short slugged tail of the URL as a human-readable hint
// when an operator goes spelunking on disk; the hash makes the dir unique.
func (s *Service) cacheDirFor(u *store.SkillUpstream) string {
	root := s.cfg.UpstreamCacheDir
	if root == "" {
		root = "upstream-cache"
	}
	h := sha256.Sum256([]byte(u.GitURL + "\x00" + u.GitSubpath))
	hint := urlHint(u.GitURL)
	return filepath.Join(root, hint+"-"+hex.EncodeToString(h[:])[:16])
}

// urlHint returns a short, filename-safe trailing slug of a git URL — used
// only as a human hint, never as an identity. Empty input → "repo".
func urlHint(gitURL string) string {
	s := strings.TrimSuffix(gitURL, ".git")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "repo"
	}
	// Replace anything that isn't alnum/dash/underscore.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return string(out)
}

// strDeref returns *p or "" if p is nil.
func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// prometheusTimer records the elapsed seconds into the checkDuration
// histogram on call. Returned as a closure so callers can stick it in a
// `defer` without taking the time twice.
func prometheusTimer() func() {
	start := time.Now()
	return func() {
		checkDuration.Observe(time.Since(start).Seconds())
	}
}
