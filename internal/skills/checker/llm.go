package checker

// llm.go classifies a Drift Detection into a structured DriftReport. When the
// LLM client is configured we send a closed-rubric prompt and parse a tight
// JSON envelope; when the LLM is unavailable, errors out, or returns an
// invalid envelope we synthesize a deterministic fallback from the diff
// metadata Detect already produced.
//
// Why structured JSON: Classify runs from the cron path — there's no human in
// the loop to interpret prose. We need stable enum-shaped severity for
// metrics, and a Summary/RecommendedAction pair the UI can render verbatim.
//
// Why fallback never returns an error: a transient LLM outage shouldn't make
// the cron cycle fail noisily. We log the underlying cause and downgrade to a
// "unknown" severity report. Operators see the row exists, the diff stats are
// preserved, and the canned RecommendedAction tells them to look manually.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/llm"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// validSeverities is the closed enum the LLM is asked to pick from. Anything
// outside this set in the model's response triggers a fallback. "unknown" is
// also the fallback severity, so a low-confidence model answer collapses to
// the same row shape as a network failure.
var validSeverities = map[string]bool{
	"cosmetic": true,
	"minor":    true,
	"major":    true,
	"security": true,
	"unknown":  true,
}

// fallbackRecommendedAction is the canned action emitted whenever the LLM
// path doesn't produce a usable answer. Centralised so tests can assert the
// exact string and operators can grep for it in dashboards.
const fallbackRecommendedAction = "Review upstream commits manually before pulling."

// classifierSystemPrompt is the role + rubric. Keeping it short keeps token
// cost predictable; the rubric is the same one the spec lists so the model's
// labels match what the UI/metrics expect.
const classifierSystemPrompt = `You classify the impact of upstream changes to a published Claude Code skill.

Respond with JSON only, in this exact shape:
{"severity": "<one of: cosmetic, minor, major, security, unknown>", "summary": "<one or two sentences>", "recommended_action": "<one short imperative sentence>"}

Severity rubric:
- cosmetic: typo fixes, formatting, comment-only edits
- minor: documentation tweaks, non-functional refactors, small clarifications
- major: behavior changes, new features, breaking changes
- security: vulnerability fixes, auth-related changes, credential handling
- unknown: ambiguous or insufficient context to choose

Be concise. Do not include explanations outside the JSON.`

// llmEnvelope is the parse target for the model's reply. Field tags match the
// shape advertised in classifierSystemPrompt; any drift between prompt and
// struct is a bug.
type llmEnvelope struct {
	Severity          string `json:"severity"`
	Summary           string `json:"summary"`
	RecommendedAction string `json:"recommended_action"`
}

// Classify converts a Detect result into a DriftReport. Identity fields
// (DetectedAt, RelayVersion, RelayHash, UpstreamSHA, UpstreamHash,
// CommitsAhead) are filled the same way regardless of which path produced
// the severity/summary/action triple.
//
// The function intentionally returns no error from the LLM path — every
// LLM-side failure mode is logged and downgraded to the offline fallback.
// Errors are reserved for genuine programmer mistakes (nil skill, nil
// detection) so a caller can fail-fast in tests.
func Classify(
	ctx context.Context,
	client *llm.Client,
	skill *store.Skill,
	det *Detection,
	relayHash, relayVersion string,
) (*store.DriftReport, error) {
	if skill == nil {
		return nil, fmt.Errorf("Classify: skill must not be nil")
	}
	if det == nil {
		return nil, fmt.Errorf("Classify: detection must not be nil")
	}

	report := &store.DriftReport{
		DetectedAt:   time.Now().UTC(),
		RelayVersion: relayVersion,
		RelayHash:    relayHash,
		UpstreamSHA:  det.NewSHA,
		UpstreamHash: det.NewHash,
		CommitsAhead: det.CommitsAhead,
	}

	severity, summary, action, model, ok := classifyWithLLM(ctx, client, skill, det)
	if !ok {
		severity, summary, action = fallbackTriple(skill, det)
		model = ""
	}
	report.Severity = severity
	report.Summary = summary
	report.RecommendedAction = action
	report.LLMModel = model
	return report, nil
}

// classifyWithLLM runs the LLM path. Returns ok=false on every failure mode
// (no client, unavailable, transport error, invalid JSON, empty fields,
// off-rubric severity); the caller falls back uniformly. ok=true guarantees
// all three returned strings are non-empty and severity is in the rubric.
func classifyWithLLM(
	ctx context.Context,
	client *llm.Client,
	skill *store.Skill,
	det *Detection,
) (severity, summary, action, model string, ok bool) {
	if client == nil || !client.Available() {
		return "", "", "", "", false
	}

	system, user := buildPrompt(skill, det)
	res, err := client.Complete(ctx, system, user)
	if err != nil {
		slog.Warn("classify: LLM call failed; falling back",
			"skill_slug", skill.Slug, "err", err)
		return "", "", "", "", false
	}

	sev, sum, act, parseOK := parseLLMResponse(res.Text)
	if !parseOK {
		slog.Warn("classify: LLM returned invalid envelope; falling back",
			"skill_slug", skill.Slug, "raw", truncate(res.Text, 256))
		return "", "", "", "", false
	}
	return sev, sum, act, client.Model(), true
}

// buildPrompt returns (system, user). The system prompt is the closed rubric;
// the user prompt is the per-skill diff context. Keeping them split lets the
// model cache the system prompt across skills in a single cycle.
func buildPrompt(skill *store.Skill, det *Detection) (string, string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Skill slug: %s\n", skill.Slug)
	if skill.DisplayName != "" {
		fmt.Fprintf(&b, "Display name: %s\n", skill.DisplayName)
	}
	if skill.LatestVersion != "" {
		fmt.Fprintf(&b, "Published version: %s\n", skill.LatestVersion)
	}
	fmt.Fprintf(&b, "Commits ahead of published: %d\n", det.CommitsAhead)

	if len(det.ChangedFiles) > 0 {
		b.WriteString("Changed files:\n")
		for _, f := range det.ChangedFiles {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	} else {
		b.WriteString("Changed files: (none reported)\n")
	}

	if det.DiffSummary != "" {
		b.WriteString("\nDiff summary (`git diff --stat`):\n")
		b.WriteString(det.DiffSummary)
		if !strings.HasSuffix(det.DiffSummary, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("\nRespond with JSON only.")
	return classifierSystemPrompt, b.String()
}

// parseLLMResponse extracts an llmEnvelope from raw model text. Tolerates
// surrounding whitespace and a single ```json fence, both of which gpt-4o-mini
// occasionally emits despite being told to send JSON only. Returns ok=false
// if any field is empty or severity is off-rubric.
func parseLLMResponse(raw string) (severity, summary, action string, ok bool) {
	s := strings.TrimSpace(raw)
	// Strip triple-backtick fences (with or without a "json" tag).
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	var env llmEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return "", "", "", false
	}
	sev := strings.ToLower(strings.TrimSpace(env.Severity))
	sum := strings.TrimSpace(env.Summary)
	act := strings.TrimSpace(env.RecommendedAction)
	if !validSeverities[sev] || sum == "" || act == "" {
		return "", "", "", false
	}
	return sev, sum, act, true
}

// fallbackTriple synthesises severity/summary/action from purely local data —
// no LLM, no network. The summary mentions both counts so an operator can
// triage the row at a glance; severity is "unknown" because we deliberately
// have no judgement to apply. RecommendedAction is the canned constant so the
// UI can dedupe "no-LLM rows" with a single equality check.
func fallbackTriple(skill *store.Skill, det *Detection) (severity, summary, action string) {
	_ = skill // reserved for future use — keeping the signature stable.
	summary = fmt.Sprintf("%d commits modified %d files. See diff summary for details.",
		det.CommitsAhead, len(det.ChangedFiles))
	return "unknown", summary, fallbackRecommendedAction
}

// truncate is a logging helper — long raw model output blows up structured
// log lines, so we cap the "raw" field at a readable length.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
