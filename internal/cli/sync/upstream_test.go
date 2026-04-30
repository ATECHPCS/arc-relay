package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/cli/sync"
)

// TestLoadUpstream_Missing — no .arc-sync/upstream.toml at all → (nil, nil).
// The push path treats this as "no upstream metadata", which is the most
// common case for skills that haven't opted into update tracking.
func TestLoadUpstream_Missing(t *testing.T) {
	u, err := sync.LoadUpstream(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing sidecar, got: %v", err)
	}
	if u != nil {
		t.Fatalf("expected nil Upstream for missing sidecar, got: %+v", u)
	}
}

// TestLoadUpstream_Valid — happy path: full sidecar with all four fields,
// confirms type defaulting is harmless when explicitly set to "git".
func TestLoadUpstream_Valid(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `
[upstream]
type    = "git"
url     = "https://github.com/foo/bar"
subpath = "skills/baz"
ref     = "main"
`
	if err := os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	u, err := sync.LoadUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected Upstream, got nil")
	}
	if u.Type != "git" || u.URL != "https://github.com/foo/bar" || u.Subpath != "skills/baz" || u.Ref != "main" {
		t.Errorf("unexpected: %+v", u)
	}
}

// TestLoadUpstream_DefaultType — type omitted; LoadUpstream defaults to "git".
// This matters because the relay enforces type=="" || type=="git" before
// recording, so a missing field shouldn't reject the sidecar.
func TestLoadUpstream_DefaultType(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `
[upstream]
url = "https://github.com/foo/bar"
`
	if err := os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	u, err := sync.LoadUpstream(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Type != "git" {
		t.Errorf("expected Type=git default, got: %+v", u)
	}
}

// TestLoadUpstream_Malformed — invalid TOML returns a non-nil error rather
// than silently dropping the sidecar. Operators want to know the file is broken.
func TestLoadUpstream_Malformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "this is = not = valid toml [["
	if err := os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	u, err := sync.LoadUpstream(dir)
	if err == nil {
		t.Fatalf("expected error for malformed TOML, got Upstream=%+v", u)
	}
	if u != nil {
		t.Errorf("expected nil Upstream alongside error, got: %+v", u)
	}
}

// TestLoadUpstream_MissingURL — the file exists but `url` is empty. The relay
// itself rejects empty-url upstreams; surface that as a parse-time error so
// the operator finds out before the push request fires.
func TestLoadUpstream_MissingURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `
[upstream]
type = "git"
ref  = "main"
`
	if err := os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	u, err := sync.LoadUpstream(dir)
	if err == nil {
		t.Fatalf("expected error for missing url, got Upstream=%+v", u)
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error should mention url, got: %v", err)
	}
}

// TestUpstream_Overrides — non-empty override strings replace existing fields;
// empty strings leave the prior value alone.
func TestUpstream_Overrides(t *testing.T) {
	u := &sync.Upstream{Type: "git", URL: "old", Ref: "main"}
	o := u.WithOverrides("new", "sub", "develop", false)
	if o.URL != "new" || o.Subpath != "sub" || o.Ref != "develop" {
		t.Errorf("override not applied: %+v", o)
	}
	// Type should still be carried over.
	if o.Type != "git" {
		t.Errorf("expected Type carried over, got: %q", o.Type)
	}
}

// TestUpstream_Overrides_PreserveUnset — an empty override string MUST NOT
// blank an existing field; that's how flag-not-passed is encoded.
func TestUpstream_Overrides_PreserveUnset(t *testing.T) {
	u := &sync.Upstream{Type: "git", URL: "keep", Subpath: "keep-sub", Ref: "keep-ref"}
	o := u.WithOverrides("", "", "", false)
	if o.URL != "keep" || o.Subpath != "keep-sub" || o.Ref != "keep-ref" || o.Type != "git" {
		t.Errorf("empty overrides clobbered fields: %+v", o)
	}
}

// TestUpstream_Overrides_ClearSentinel — clearAll=true returns the
// "clear me" sentinel: empty Type AND empty URL. The wire layer reads
// that pair to decide between X-Upstream and X-Clear-Upstream headers.
func TestUpstream_Overrides_ClearSentinel(t *testing.T) {
	u := &sync.Upstream{Type: "git", URL: "old", Ref: "main"}
	o := u.WithOverrides("anything", "anything", "anything", true)
	if o == nil {
		t.Fatal("expected non-nil sentinel, got nil")
	}
	if o.Type != "" || o.URL != "" {
		t.Errorf("clear sentinel should be zero-value (Type=URL=\"\"), got: %+v", o)
	}
}

// TestUpstream_LoadAndMerge_NoUpstream — --no-upstream short-circuits:
// returns the clear sentinel without touching the sidecar (so a malformed
// sidecar doesn't block the operator from clearing).
func TestUpstream_LoadAndMerge_NoUpstream(t *testing.T) {
	dir := t.TempDir()
	// Plant a malformed sidecar — proves we don't read it.
	if err := os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte("garbage[["), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	u, err := sync.LoadAndMerge(dir, "", "", "", true)
	if err != nil {
		t.Fatalf("--no-upstream should not surface sidecar errors: %v", err)
	}
	if u == nil {
		t.Fatal("expected clear sentinel, got nil")
	}
	if u.Type != "" || u.URL != "" {
		t.Errorf("expected clear sentinel, got: %+v", u)
	}
}

// TestUpstream_LoadAndMerge_NoSidecarNoFlags — no sidecar, no flags: nil
// result. The push request should send neither X-Upstream nor
// X-Clear-Upstream.
func TestUpstream_LoadAndMerge_NoSidecarNoFlags(t *testing.T) {
	u, err := sync.LoadAndMerge(t.TempDir(), "", "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != nil {
		t.Errorf("expected nil result, got: %+v", u)
	}
}

// TestUpstream_LoadAndMerge_FlagsOnlySeed — no sidecar, but operator passed
// --upstream-git: seed an Upstream from flags alone.
func TestUpstream_LoadAndMerge_FlagsOnlySeed(t *testing.T) {
	u, err := sync.LoadAndMerge(t.TempDir(), "https://example.com/repo", "skills/foo", "v1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u == nil {
		t.Fatal("expected Upstream from flags, got nil")
	}
	if u.Type != "git" || u.URL != "https://example.com/repo" || u.Subpath != "skills/foo" || u.Ref != "v1" {
		t.Errorf("flag-seeded upstream wrong: %+v", u)
	}
}

// TestUpstream_LoadAndMerge_SidecarPlusFlags — flags override sidecar fields,
// others stay.
func TestUpstream_LoadAndMerge_SidecarPlusFlags(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".arc-sync"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `
[upstream]
url     = "https://github.com/foo/bar"
subpath = "skills/baz"
ref     = "main"
`
	if err := os.WriteFile(filepath.Join(dir, ".arc-sync", "upstream.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	u, err := sync.LoadAndMerge(dir, "", "", "develop", false)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected Upstream, got nil")
	}
	if u.URL != "https://github.com/foo/bar" || u.Subpath != "skills/baz" {
		t.Errorf("sidecar fields should survive: %+v", u)
	}
	if u.Ref != "develop" {
		t.Errorf("--upstream-ref override didn't take: %+v", u)
	}
}

// TestUpstream_ToWire — non-clear Upstream maps cleanly to the relay wire shape.
func TestUpstream_ToWire(t *testing.T) {
	u := &sync.Upstream{Type: "git", URL: "u", Subpath: "s", Ref: "r"}
	w := u.ToWire()
	if w == nil {
		t.Fatal("expected wire shape, got nil")
	}
	if w.Type != "git" || w.URL != "u" || w.Subpath != "s" || w.Ref != "r" {
		t.Errorf("wire shape wrong: %+v", w)
	}
}

// TestUpstream_ToWire_ClearSentinel — clear sentinel converts but its empty
// Type+URL signal the upload layer to send X-Clear-Upstream instead.
func TestUpstream_ToWire_ClearSentinel(t *testing.T) {
	u := &sync.Upstream{}
	w := u.ToWire()
	if w == nil {
		t.Fatal("expected wire shape, got nil")
	}
	if w.Type != "" || w.URL != "" {
		t.Errorf("clear sentinel wire shape should be empty, got: %+v", w)
	}
}
