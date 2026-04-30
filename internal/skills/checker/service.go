package checker

// service.go declares the Service type that drives upstream-update detection.
// It composes the lower-level git helpers (git.go) and Detect() (detect.go)
// against a SkillStore for persistence and a config.SkillsCheckerConfig for
// runtime knobs (cache directory, diff size cap, timeouts).
//
// The LLM client is wired through NewService for forward-compatibility with
// Task 11 — Task 9 itself only emits placeholder severity/summary on Drift
// and never calls the LLM.

import (
	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// Service is the checker's main entrypoint: cron loop, on-demand single-skill
// check (Task 12), and the per-upstream orchestration in checkOne.
type Service struct {
	skills *store.SkillStore
	llm    *llm.Client
	cfg    config.SkillsCheckerConfig
}

// NewService constructs a Service. llm may be nil-but-non-Available; checkOne
// only consults it on the Drift path (Task 11) and currently emits placeholder
// values regardless.
func NewService(skills *store.SkillStore, llm *llm.Client, cfg config.SkillsCheckerConfig) *Service {
	return &Service{
		skills: skills,
		llm:    llm,
		cfg:    cfg,
	}
}
