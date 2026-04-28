package sync_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/cli/relay"
	"github.com/comma-compliance/arc-relay/internal/cli/sync"
)

// buildSkillArchive creates a minimal gzipped tar with a SKILL.md +
// optional extra files. Each file is {name, contents}.
func buildSkillArchive(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{
			Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

const goodSkillMD = `---
name: hello-skill
description: Says hello.
---

# Hello
`

// fakeRelay is a tiny httptest server that mimics the parts of the relay API
// the skill manager calls: GET /api/skills/assigned and GET /api/skills/{slug}/versions/{version}/archive.
type fakeRelay struct {
	*httptest.Server
	assigned []*relay.AssignedSkill
	archives map[string][]byte // key = "slug/version"
	skills   map[string]*relay.SkillDetail
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	fr := &fakeRelay{
		archives: map[string][]byte{},
		skills:   map[string]*relay.SkillDetail{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/skills/assigned", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"assigned": fr.assigned})
	})
	mux.HandleFunc("/api/skills/", func(w http.ResponseWriter, r *http.Request) {
		// /api/skills/{slug} or /api/skills/{slug}/versions/{version}/archive
		rest := strings.TrimPrefix(r.URL.Path, "/api/skills/")
		parts := strings.Split(rest, "/")
		slug := parts[0]
		switch {
		case len(parts) == 1:
			d, ok := fr.skills[slug]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(d)
		case len(parts) == 4 && parts[1] == "versions" && parts[3] == "archive":
			version := parts[2]
			arch, ok := fr.archives[slug+"/"+version]
			if !ok {
				http.NotFound(w, r)
				return
			}
			sum := sha256.Sum256(arch)
			w.Header().Set("X-Skill-SHA256", hex.EncodeToString(sum[:]))
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(arch)
		default:
			http.NotFound(w, r)
		}
	})
	fr.Server = httptest.NewServer(mux)
	t.Cleanup(fr.Close)
	return fr
}

func (fr *fakeRelay) addSkill(slug, version string, archive []byte) {
	fr.archives[slug+"/"+version] = archive
	fr.skills[slug] = &relay.SkillDetail{
		Skill:    &relay.Skill{Slug: slug, LatestVersion: version},
		Versions: []*relay.SkillVersion{{Version: version}},
	}
}

func newManager(t *testing.T, fr *fakeRelay) *sync.SkillManager {
	t.Helper()
	return &sync.SkillManager{
		Client: &relay.Client{
			BaseURL:    fr.URL,
			APIKey:     "test-key",
			HTTPClient: fr.Client(),
		},
		SkillsDir: t.TempDir(),
	}
}

func TestSkillInstall_HappyPath(t *testing.T) {
	fr := newFakeRelay(t)
	archive := buildSkillArchive(t, [][2]string{
		{"SKILL.md", goodSkillMD},
		{"helpers/util.sh", "#!/bin/sh\necho hi\n"},
	})
	fr.addSkill("hello-skill", "1.0.0", archive)
	mgr := newManager(t, fr)

	marker, err := mgr.Install("hello-skill", "1.0.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if marker.Slug != "hello-skill" {
		t.Errorf("marker.Slug = %q", marker.Slug)
	}

	// SKILL.md extracted at root.
	if _, err := os.Stat(filepath.Join(mgr.SkillsDir, "hello-skill", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing: %v", err)
	}
	// Subfolder file extracted.
	if _, err := os.Stat(filepath.Join(mgr.SkillsDir, "hello-skill", "helpers", "util.sh")); err != nil {
		t.Errorf("helpers/util.sh missing: %v", err)
	}
	// Marker written.
	markerPath := filepath.Join(mgr.SkillsDir, "hello-skill", sync.SkillMarkerFile)
	b, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var got sync.SkillMarker
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if got.Version != "1.0.0" || got.Slug != "hello-skill" {
		t.Errorf("marker = %+v", got)
	}
}

func TestSkillInstall_RefusesHandInstalled(t *testing.T) {
	fr := newFakeRelay(t)
	fr.addSkill("hello-skill", "1.0.0", buildSkillArchive(t, [][2]string{{"SKILL.md", goodSkillMD}}))
	mgr := newManager(t, fr)

	// Pre-create a hand-installed dir with no marker.
	dest := filepath.Join(mgr.SkillsDir, "hello-skill")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte("# hand"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := mgr.Install("hello-skill", "1.0.0"); err == nil {
		t.Fatal("Install should refuse to overwrite hand-installed skill")
	}
	// Hand-installed content untouched.
	got, _ := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if string(got) != "# hand" {
		t.Errorf("hand-installed file overwritten: %q", got)
	}
}

func TestSkillInstall_UpgradeReplacesMarker(t *testing.T) {
	fr := newFakeRelay(t)
	v1 := buildSkillArchive(t, [][2]string{{"SKILL.md", goodSkillMD + "\nv1\n"}})
	v2 := buildSkillArchive(t, [][2]string{{"SKILL.md", goodSkillMD + "\nv2\n"}})
	fr.addSkill("hello-skill", "1.0.0", v1)
	fr.archives["hello-skill/2.0.0"] = v2
	mgr := newManager(t, fr)

	if _, err := mgr.Install("hello-skill", "1.0.0"); err != nil {
		t.Fatalf("install v1: %v", err)
	}
	if _, err := mgr.Install("hello-skill", "2.0.0"); err != nil {
		t.Fatalf("install v2: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(mgr.SkillsDir, "hello-skill", "SKILL.md"))
	if !strings.Contains(string(b), "v2") {
		t.Errorf("upgrade did not replace SKILL.md: %q", b)
	}
	// Old version's SHA shouldn't survive in marker.
	mb, _ := os.ReadFile(filepath.Join(mgr.SkillsDir, "hello-skill", sync.SkillMarkerFile))
	if !strings.Contains(string(mb), `"version": "2.0.0"`) {
		t.Errorf("marker not updated: %s", mb)
	}
}

func TestSkillRemove_Idempotent(t *testing.T) {
	mgr := &sync.SkillManager{SkillsDir: t.TempDir()}
	if err := mgr.Remove("does-not-exist"); err != nil {
		t.Errorf("Remove on missing slug: %v", err)
	}
}

func TestSkillRemove_RefusesHandInstalled(t *testing.T) {
	mgr := &sync.SkillManager{SkillsDir: t.TempDir()}
	dest := filepath.Join(mgr.SkillsDir, "manual")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte("# manual"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := mgr.Remove("manual")
	if err == nil {
		t.Fatal("Remove should refuse hand-installed skill")
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Errorf("hand-installed file deleted: %v", err)
	}
}

func TestSkillSync_InstallsAndRemoves(t *testing.T) {
	fr := newFakeRelay(t)
	fr.addSkill("a-skill", "1.0.0", buildSkillArchive(t, [][2]string{{"SKILL.md",
		"---\nname: a-skill\n---\n"}}))
	fr.addSkill("b-skill", "2.0.0", buildSkillArchive(t, [][2]string{{"SKILL.md",
		"---\nname: b-skill\n---\n"}}))
	fr.assigned = []*relay.AssignedSkill{
		{Skill: fr.skills["a-skill"].Skill},
		{Skill: fr.skills["b-skill"].Skill},
	}
	mgr := newManager(t, fr)

	report, err := mgr.Sync(sync.SkillSyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(report.Installed) != 2 {
		t.Errorf("Installed = %d, want 2 (%+v)", len(report.Installed), report)
	}
	if len(report.Errors) != 0 {
		t.Errorf("Errors = %+v", report.Errors)
	}

	// Drop a-skill from assigned. Re-sync should remove it locally.
	fr.assigned = []*relay.AssignedSkill{{Skill: fr.skills["b-skill"].Skill}}
	report2, err := mgr.Sync(sync.SkillSyncOptions{})
	if err != nil {
		t.Fatalf("re-Sync: %v", err)
	}
	if len(report2.Removed) != 1 || report2.Removed[0].Slug != "a-skill" {
		t.Errorf("expected a-skill removed, got %+v", report2.Removed)
	}
	if _, err := os.Stat(filepath.Join(mgr.SkillsDir, "a-skill")); !os.IsNotExist(err) {
		t.Errorf("a-skill dir should be gone, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(mgr.SkillsDir, "b-skill")); err != nil {
		t.Errorf("b-skill should remain: %v", err)
	}
}

func TestSkillSync_LeavesHandInstalledAlone(t *testing.T) {
	fr := newFakeRelay(t)
	fr.assigned = []*relay.AssignedSkill{}
	mgr := newManager(t, fr)

	// Plant a hand-installed skill that arc-sync should never touch.
	dest := filepath.Join(mgr.SkillsDir, "manual")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte("# manual"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	report, err := mgr.Sync(sync.SkillSyncOptions{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(report.SkippedHand) != 1 {
		t.Errorf("expected SkippedHand=1, got %+v", report)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("hand-installed dir vanished: %v", err)
	}
}

func TestSkillSync_DryRunDoesNothing(t *testing.T) {
	fr := newFakeRelay(t)
	fr.addSkill("a-skill", "1.0.0", buildSkillArchive(t, [][2]string{{"SKILL.md",
		"---\nname: a-skill\n---\n"}}))
	fr.assigned = []*relay.AssignedSkill{{Skill: fr.skills["a-skill"].Skill}}
	mgr := newManager(t, fr)

	report, err := mgr.Sync(sync.SkillSyncOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Sync dry-run: %v", err)
	}
	if len(report.Installed) != 1 {
		t.Errorf("dry-run should report Installed=1, got %+v", report)
	}
	if _, err := os.Stat(filepath.Join(mgr.SkillsDir, "a-skill")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote files: %v", err)
	}
}

func TestSkillSync_PinnedVersionWins(t *testing.T) {
	fr := newFakeRelay(t)
	v1 := buildSkillArchive(t, [][2]string{{"SKILL.md", "---\nname: a\n---\nv1"}})
	v2 := buildSkillArchive(t, [][2]string{{"SKILL.md", "---\nname: a\n---\nv2"}})
	fr.archives["a/1.0.0"] = v1
	fr.archives["a/2.0.0"] = v2
	pin := "1.0.0"
	fr.skills["a"] = &relay.SkillDetail{Skill: &relay.Skill{Slug: "a", LatestVersion: "2.0.0"}}
	fr.assigned = []*relay.AssignedSkill{{
		Skill: fr.skills["a"].Skill, PinnedVersion: &pin,
	}}
	mgr := newManager(t, fr)

	if _, err := mgr.Sync(sync.SkillSyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	mb, _ := os.ReadFile(filepath.Join(mgr.SkillsDir, "a", sync.SkillMarkerFile))
	if !strings.Contains(string(mb), `"version": "1.0.0"`) {
		t.Errorf("expected pinned version 1.0.0 installed, marker=%s", mb)
	}
}

func TestPackageSkill_RoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"),
		[]byte(goodSkillMD), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "helpers"), 0o755); err != nil {
		t.Fatalf("mkdir helpers: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "helpers", "util.sh"),
		[]byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed util: %v", err)
	}
	// Hidden file we expect PackageSkill to skip.
	if err := os.WriteFile(filepath.Join(src, ".arc-sync-version"),
		[]byte(`{"slug":"x"}`), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	archive, slug, err := sync.PackageSkill(src)
	if err != nil {
		t.Fatalf("PackageSkill: %v", err)
	}
	if slug != "hello-skill" {
		t.Errorf("slug = %q", slug)
	}
	// Round-trip: extract and check files.
	dest := t.TempDir()
	mgr := &sync.SkillManager{SkillsDir: dest}
	// Cheat: mimic Install's extract by writing the archive into the dest
	// directory through the public surface.
	fr := newFakeRelay(t)
	fr.addSkill("hello-skill", "1.0.0", archive)
	mgr.Client = &relay.Client{BaseURL: fr.URL, APIKey: "x", HTTPClient: fr.Client()}

	if _, err := mgr.Install("hello-skill", "1.0.0"); err != nil {
		t.Fatalf("Install of packaged archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "hello-skill", "helpers", "util.sh")); err != nil {
		t.Errorf("packaged util.sh missing: %v", err)
	}
	// .arc-sync-version from the SOURCE should NOT have been packaged.
	// The Install path will write a fresh marker. To check the package didn't
	// include the source's marker, look for a file the source had but with
	// distinct contents — open the package archive directly.
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Name == ".arc-sync-version" {
			t.Errorf("PackageSkill included hidden marker file")
		}
	}
}

func TestExtractTarGz_RejectsTraversal(t *testing.T) {
	bad := buildSkillArchive(t, [][2]string{
		{"SKILL.md", goodSkillMD},
		{"../escape.txt", "haha"},
	})
	fr := newFakeRelay(t)
	fr.addSkill("hello-skill", "1.0.0", bad)
	mgr := newManager(t, fr)

	if _, err := mgr.Install("hello-skill", "1.0.0"); err == nil {
		t.Fatal("Install should reject traversal entry")
	}
	// Nothing should be extracted.
	if _, err := os.Stat(filepath.Join(mgr.SkillsDir, "hello-skill")); !os.IsNotExist(err) {
		t.Errorf("partial extract left behind: %v", err)
	}
}

func TestSkillInstall_RejectsCorruptArchive(t *testing.T) {
	fr := newFakeRelay(t)
	fr.archives["hello-skill/1.0.0"] = []byte("not a gzip")
	fr.skills["hello-skill"] = &relay.SkillDetail{Skill: &relay.Skill{Slug: "hello-skill", LatestVersion: "1.0.0"}}
	mgr := newManager(t, fr)

	if _, err := mgr.Install("hello-skill", "1.0.0"); err == nil {
		t.Fatal("Install should fail on corrupt archive")
	}
}
