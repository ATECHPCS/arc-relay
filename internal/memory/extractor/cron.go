package extractor

import (
	"context"
	"log/slog"
	"time"
)

// RunCron starts the periodic backstop loop. Every `interval`, the loop
// asks the session store for stale sessions (last_seen_at >1h ago AND
// last_extracted_at NULL or behind last_seen_at) and runs Extract on each.
//
// This is the safety net for sessions where the watcher quiescence push
// never fired (machine sleep, crash, network drop). The cron's 1-hour
// staleness threshold deliberately doesn't compete with the watcher's 60s
// quiescence path — sessions extract via the watcher first, cron only
// catches strays.
//
// Returns when ctx is canceled. Each cycle's outcome is logged at INFO.
func (s *Service) RunCron(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	slog.Info("extractor cron loop started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			slog.Info("extractor cron loop stopped")
			return
		case <-t.C:
			s.cronCycle(ctx)
		}
	}
}

const cronBatchSize = 50

// cronCycle pulls one batch of stale sessions and extracts each. Errors are
// logged but don't abort the cycle — one bad session shouldn't block others.
func (s *Service) cronCycle(ctx context.Context) {
	start := time.Now()
	sessions, err := s.sessions.ListStaleForExtraction(cronBatchSize)
	if err != nil {
		slog.Error("cron: list stale failed", "err", err)
		return
	}
	if len(sessions) == 0 {
		slog.Debug("cron cycle: no stale sessions")
		return
	}

	var ok, fail int
	for _, sid := range sessions {
		// Per-session timeout via context — Extract uses its own per-call
		// timeout (180s) per chunk plus one retry on transient errors; this
		// is just a circuit breaker on the whole extraction flow. 15 min
		// fits a 30-chunk session at worst-case 30s/chunk with retries.
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		_, err := s.Extract(callCtx, sid)
		cancel()
		if err != nil {
			slog.Error("cron: extract failed", "session", sid, "err", err)
			fail++
		} else {
			ok++
		}
	}

	slog.Info("cron cycle complete",
		"picked", len(sessions),
		"ok", ok,
		"fail", fail,
		"ms", time.Since(start).Milliseconds())
}
