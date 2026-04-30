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
	"errors"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// ErrSkillNotFound is returned by RunOneSlug when no skill with the given slug
// exists. The HTTP layer maps this to 404. Distinct from OutcomeNoUpstream
// (which means the skill exists but has no skill_upstreams row).
var ErrSkillNotFound = errors.New("skill not found")

// CheckOutcome classifies the result of an on-demand single-skill check
// (RunOneSlug). It exists so the HTTP endpoint can produce a meaningful status
// code without duplicating the cron's Detect-result switch.
type CheckOutcome int

const (
	// OutcomeUpToDate covers the three "no drift" Detect results:
	// NoMovement, NoPathTouch, RevertedToSame.
	OutcomeUpToDate CheckOutcome = iota
	// OutcomeDrift means Detect returned ResultDrift; the drift_* columns
	// have been populated and skills.outdated has been flipped on.
	OutcomeDrift
	// OutcomeNoUpstream means the skill exists but has no skill_upstreams
	// row — there is nothing to check against. HTTP maps this to 409.
	OutcomeNoUpstream
	// OutcomeFetchFailed means EnsureCache or Detect failed (network,
	// permissions, malformed ref, etc.). HTTP maps this to 502.
	OutcomeFetchFailed
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
