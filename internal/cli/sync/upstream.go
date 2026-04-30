package sync

// Upstream tracking for `arc-sync skill push`. The operator records where a
// skill came from upstream (e.g. a GitHub repo) so the relay's drift checker
// (Phase 3) can compare published archives against their source. Metadata is
// loaded from `.arc-sync/upstream.toml` inside the skill source dir, with
// optional CLI-flag overrides on the `arc-sync skill push` command.
//
// Wire format: the relay reads this from HTTP headers on the version-upload
// POST — `X-Upstream: <JSON>` for new/updated metadata, or
// `X-Clear-Upstream: true` to disassociate a skill from its upstream.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/comma-compliance/arc-relay/internal/cli/relay"
)

// Upstream is the in-memory representation of `.arc-sync/upstream.toml`.
// The TOML file's `[upstream]` table maps directly onto these fields. All
// fields are TOML-tagged so the same struct can be the decode target for
// both the on-disk sidecar and ad-hoc construction in tests.
//
// Sentinel value (clear-upstream signal): empty Type AND empty URL. Returned
// by WithOverrides when clearAll=true; consumed by ToWire/UploadSkill to
// emit `X-Clear-Upstream: true` instead of `X-Upstream: <json>`.
type Upstream struct {
	Type    string `toml:"type"`
	URL     string `toml:"url"`
	Subpath string `toml:"subpath"`
	Ref     string `toml:"ref"`
}

// upstreamFile is the on-disk TOML decode target. The single `[upstream]`
// table keeps the file extensible — future Phase work might add
// `[upstream.notes]` or `[skill]` sections without breaking this shape.
type upstreamFile struct {
	Upstream Upstream `toml:"upstream"`
}

// LoadUpstream reads `<skillDir>/.arc-sync/upstream.toml` and returns the
// parsed Upstream. Returns (nil, nil) when the sidecar file does not exist —
// most skills won't opt into upstream tracking, so missing-file is the
// expected case, not an error.
//
// Defaults applied:
//   - Type defaults to "git" when omitted (matches the relay's accept rule:
//     it requires Type=="" || Type=="git" today).
//   - Ref is left as-is; the relay's UpsertUpstream defaults empty Ref to
//     "HEAD" server-side.
//
// Errors:
//   - Malformed TOML returns a decode error verbatim from BurntSushi/toml.
//   - Missing required `url` field returns an explicit error so the operator
//     finds out before the push request fires (the relay would silently
//     reject it with `X-Upstream rejected` log noise otherwise).
func LoadUpstream(skillDir string) (*Upstream, error) {
	p := filepath.Join(skillDir, ".arc-sync", "upstream.toml")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	var f upstreamFile
	if _, err := toml.Decode(string(b), &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	if f.Upstream.URL == "" {
		return nil, fmt.Errorf("%s: [upstream].url is required", p)
	}
	if f.Upstream.Type == "" {
		f.Upstream.Type = "git"
	}
	return &f.Upstream, nil
}

// WithOverrides returns a copy of u with non-empty override strings replacing
// matching fields. Empty override strings leave the existing field untouched
// — that's how "flag not passed" is encoded by the CLI layer.
//
// clearAll=true short-circuits everything and returns a zero-value Upstream
// (the clear sentinel). Callers must check `u.Type == "" && u.URL == ""` to
// distinguish the sentinel from a normal value.
//
// u may be nil-receiver-safe in pseudocode, but we don't call it on nil — the
// CLI layer constructs an empty Upstream{Type: "git"} first if no sidecar is
// present.
func (u *Upstream) WithOverrides(url, subpath, ref string, clearAll bool) *Upstream {
	if clearAll {
		return &Upstream{} // zero value signals "clear"
	}
	out := *u
	if url != "" {
		out.URL = url
	}
	if subpath != "" {
		out.Subpath = subpath
	}
	if ref != "" {
		out.Ref = ref
	}
	return &out
}

// ToWire converts an Upstream into the relay client's wire shape.
// The wire type lives in `internal/cli/relay` so `relay.UploadSkill` can
// accept it without importing `internal/cli/sync` (sync already imports
// relay; the reverse would cycle).
//
// For the clear sentinel (empty Type + URL), this still returns a non-nil
// pointer with empty fields — the relay client uses that pair to decide
// between `X-Upstream` and `X-Clear-Upstream`. Callers that already know
// they want the clear path can pass `&relay.UpstreamMetadata{}` directly.
func (u *Upstream) ToWire() *relay.UpstreamMetadata {
	if u == nil {
		return nil
	}
	return &relay.UpstreamMetadata{
		Type:    u.Type,
		URL:     u.URL,
		Subpath: u.Subpath,
		Ref:     u.Ref,
	}
}

// LoadAndMerge wraps LoadUpstream + WithOverrides for the CLI layer. Returns:
//   - clear sentinel when noUpstream=true (skipping the sidecar entirely so a
//     malformed file can't block the operator from clearing).
//   - merged Upstream (sidecar + overrides) when sidecar exists or any
//     --upstream-* flag is non-empty.
//   - (nil, nil) when no sidecar AND no flags — push proceeds without any
//     upstream-related headers.
//
// urlFlag is the seed flag: when no sidecar exists, a non-empty urlFlag
// alone bootstraps an Upstream{Type: "git"} skeleton. subpathFlag/refFlag
// without urlFlag don't seed (no URL means the wire layer would reject it).
func LoadAndMerge(skillDir, urlFlag, subpathFlag, refFlag string, noUpstream bool) (*Upstream, error) {
	if noUpstream {
		return (&Upstream{}).WithOverrides("", "", "", true), nil
	}
	side, err := LoadUpstream(skillDir)
	if err != nil {
		return nil, err
	}
	// No sidecar AND no flags → nothing to send.
	if side == nil && urlFlag == "" && subpathFlag == "" && refFlag == "" {
		return nil, nil
	}
	base := side
	if base == nil {
		// Seed from flags. Type defaults to git so ToWire produces a
		// payload the relay will accept.
		base = &Upstream{Type: "git"}
	}
	return base.WithOverrides(urlFlag, subpathFlag, refFlag, false), nil
}
