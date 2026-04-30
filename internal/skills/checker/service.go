package checker

// service.go declares the Service type that drives upstream-update detection.
// It composes the lower-level git helpers (git.go) and Detect() (detect.go)
// against a SkillStore for persistence and a config.SkillsCheckerConfig for
// runtime knobs (cache directory, diff size cap, timeouts).
//
// The LLM client is wired through NewService for the Drift-classification
// path (Task 11): checkOne consults Classify() to populate the DriftReport's
// severity/summary/recommended_action fields.
//
// skillsSvc is the upstream skill repository service. checkOne uses it to
// compute the relay-side subtree hash from the latest published archive so
// the DriftReport can carry both relay and upstream hashes for UI diffing.

import (
	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// Service is the checker's main entrypoint: cron loop, on-demand single-skill
// check (Task 12), and the per-upstream orchestration in checkOne.
type Service struct {
	skills    *store.SkillStore
	skillsSvc *skills.Service
	llm       *llm.Client
	cfg       config.SkillsCheckerConfig
}

// NewService constructs a Service. llm may be nil-but-non-Available; Classify
// transparently falls back to a deterministic offline triple in that case.
// skillsSvc is used only on the Drift path (to read the published archive
// and compute its hash); it may be nil in tests that don't exercise drift.
func NewService(skills *store.SkillStore, skillsSvc *skills.Service, llm *llm.Client, cfg config.SkillsCheckerConfig) *Service {
	return &Service{
		skills:    skills,
		skillsSvc: skillsSvc,
		llm:       llm,
		cfg:       cfg,
	}
}
