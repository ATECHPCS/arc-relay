package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/cli/relay"
)

// fakeDriftClient stubs the driftClient interface so we can drive
// checkUpdates without a real httptest server. Each method is keyed off the
// slug; ListSkills returns the configured slug list.
type fakeDriftClient struct {
	skills []*relay.Skill
	drift  map[string]*relay.DriftBlock
	errors map[string]error
}

func (f *fakeDriftClient) CheckDrift(slug string) (*relay.DriftBlock, error) {
	if err, ok := f.errors[slug]; ok {
		return nil, err
	}
	return f.drift[slug], nil
}

func (f *fakeDriftClient) ListSkills() ([]*relay.Skill, error) {
	return f.skills, nil
}

func TestCheckUpdates_SingleSkill_Drift(t *testing.T) {
	c := &fakeDriftClient{
		drift: map[string]*relay.DriftBlock{
			"foo": {
				Severity:          "minor",
				Summary:           "3 commits behind",
				RecommendedAction: "run `arc-sync skill push`",
			},
		},
	}
	var stdout, stderr bytes.Buffer
	if err := checkUpdates(c, "foo", &stdout, &stderr); err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "outdated · minor: 3 commits behind") {
		t.Errorf("stdout missing summary line: %q", out)
	}
	if !strings.Contains(out, "run `arc-sync skill push`") {
		t.Errorf("stdout missing recommended action: %q", out)
	}
}

func TestCheckUpdates_SingleSkill_UpToDate(t *testing.T) {
	c := &fakeDriftClient{
		drift: map[string]*relay.DriftBlock{"foo": nil},
	}
	var stdout, stderr bytes.Buffer
	if err := checkUpdates(c, "foo", &stdout, &stderr); err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "up-to-date" {
		t.Errorf("stdout = %q, want %q", got, "up-to-date")
	}
}

func TestCheckUpdates_SingleSkill_NotFound(t *testing.T) {
	c := &fakeDriftClient{
		errors: map[string]error{
			"missing": &relay.SkillHTTPError{Status: http.StatusNotFound},
		},
	}
	var stdout, stderr bytes.Buffer
	err := checkUpdates(c, "missing", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(stderr.String(), "skill not found") {
		t.Errorf("stderr missing 'skill not found': %q", stderr.String())
	}
}

func TestCheckUpdates_SingleSkill_NoUpstream(t *testing.T) {
	c := &fakeDriftClient{
		errors: map[string]error{
			"foo": &relay.SkillHTTPError{Status: http.StatusConflict},
		},
	}
	var stdout, stderr bytes.Buffer
	err := checkUpdates(c, "foo", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 409 in single-skill mode")
	}
	if !strings.Contains(stderr.String(), "no upstream tracking configured") {
		t.Errorf("stderr missing 409 message: %q", stderr.String())
	}
}

func TestCheckUpdates_SingleSkill_UpstreamFailed(t *testing.T) {
	c := &fakeDriftClient{
		errors: map[string]error{
			"foo": &relay.SkillHTTPError{Status: http.StatusBadGateway},
		},
	}
	var stdout, stderr bytes.Buffer
	err := checkUpdates(c, "foo", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 502")
	}
	if !strings.Contains(stderr.String(), "upstream fetch failed") {
		t.Errorf("stderr missing 502 message: %q", stderr.String())
	}
}

// TestCheckUpdates_AllSkills exercises iteration mode: 409s skipped silently,
// 204s print up-to-date, drift hits print "outdated · <severity>".
func TestCheckUpdates_AllSkills(t *testing.T) {
	c := &fakeDriftClient{
		skills: []*relay.Skill{
			{Slug: "alpha"},
			{Slug: "beta"},
			{Slug: "gamma"},
		},
		drift: map[string]*relay.DriftBlock{
			"alpha": {Severity: "minor", Summary: "x"},
			// beta has no entry → returns nil = up-to-date
		},
		errors: map[string]error{
			"gamma": &relay.SkillHTTPError{Status: http.StatusConflict},
		},
	}
	var stdout, stderr bytes.Buffer
	if err := checkUpdates(c, "", &stdout, &stderr); err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "alpha: outdated · minor") {
		t.Errorf("stdout missing alpha drift line: %q", out)
	}
	if !strings.Contains(out, "beta: up-to-date") {
		t.Errorf("stdout missing beta up-to-date line: %q", out)
	}
	// gamma should be silently skipped — no line in stdout, nothing in stderr.
	if strings.Contains(out, "gamma") {
		t.Errorf("gamma should be silently skipped on 409, got: %q", out)
	}
	if strings.Contains(stderr.String(), "gamma") {
		t.Errorf("gamma should not appear in stderr on 409, got: %q", stderr.String())
	}
}

// TestPrintSkillUsage_MentionsCheckUpdates is a smoke test that the help
// output advertises the new subcommand. It also indirectly verifies that
// the dispatcher's `case "check-updates":` arm is discoverable to users.
// Direct coverage of runSkill (which reads os.Args + builds a real config)
// is out of scope for a unit test.
func TestPrintSkillUsage_MentionsCheckUpdates(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	printSkillUsage()
	_ = w.Close()
	os.Stdout = saved

	var captured bytes.Buffer
	if _, err := io.Copy(&captured, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if !strings.Contains(captured.String(), "check-updates") {
		t.Errorf("printSkillUsage output missing check-updates: %q", captured.String())
	}
}
