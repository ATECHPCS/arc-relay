package skills_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// buildArchive returns a gzipped tarball with the given files. files is a slice
// of {name, content} pairs; pass an entry with name "" to inject an unsafe
// header (used to test traversal rejection).
func buildArchive(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     f[0],
			Mode:     0o644,
			Size:     int64(len(f[1])),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	return buf.Bytes()
}

const goodSkillMD = `---
name: test-skill
description: A test skill used for unit tests.
user-invocable: true
allowed-tools: Bash(echo *)
argument-hint: [arg]
---

# Test skill body
`

func TestValidateArchive_HappyPath(t *testing.T) {
	archive := buildArchive(t, [][2]string{
		{"SKILL.md", goodSkillMD},
		{"helpers/util.sh", "#!/bin/sh\necho hi"},
	})
	m, err := skills.ValidateArchive(archive)
	if err != nil {
		t.Fatalf("ValidateArchive: %v", err)
	}
	if m.Name != "test-skill" {
		t.Errorf("Name = %q, want test-skill", m.Name)
	}
	if got, _ := m.Extra["user-invocable"].(bool); !got {
		t.Errorf("Extra[user-invocable] = %v, want true", m.Extra["user-invocable"])
	}
	if got, _ := m.Extra["allowed-tools"].(string); got != "Bash(echo *)" {
		t.Errorf("Extra[allowed-tools] = %v", m.Extra["allowed-tools"])
	}
}

func TestValidateArchive_DotPrefixedSkillMD(t *testing.T) {
	archive := buildArchive(t, [][2]string{{"./SKILL.md", goodSkillMD}})
	if _, err := skills.ValidateArchive(archive); err != nil {
		t.Fatalf("ValidateArchive ./SKILL.md: %v", err)
	}
}

func TestValidateArchive_RejectsMissingSkillMD(t *testing.T) {
	archive := buildArchive(t, [][2]string{{"README.md", "# nope"}})
	_, err := skills.ValidateArchive(archive)
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive, got %v", err)
	}
}

func TestValidateArchive_RejectsBadFrontmatter(t *testing.T) {
	bad := "---\nname: ok\nbroken: : :\n---\n"
	archive := buildArchive(t, [][2]string{{"SKILL.md", bad}})
	_, err := skills.ValidateArchive(archive)
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive on bad YAML, got %v", err)
	}
}

func TestValidateArchive_RejectsMissingFrontmatter(t *testing.T) {
	archive := buildArchive(t, [][2]string{{"SKILL.md", "# Just a body, no frontmatter\n"}})
	_, err := skills.ValidateArchive(archive)
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive, got %v", err)
	}
}

func TestValidateArchive_RejectsEmptyName(t *testing.T) {
	skillMD := "---\nname: \"\"\ndescription: x\n---\n"
	archive := buildArchive(t, [][2]string{{"SKILL.md", skillMD}})
	_, err := skills.ValidateArchive(archive)
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive, got %v", err)
	}
}

func TestValidateArchive_RejectsTraversal(t *testing.T) {
	cases := []string{"../escape.txt", "/abs/path", "sub/../../etc/passwd"}
	for _, name := range cases {
		archive := buildArchive(t, [][2]string{
			{"SKILL.md", goodSkillMD},
			{name, "x"},
		})
		_, err := skills.ValidateArchive(archive)
		if !errors.Is(err, skills.ErrInvalidArchive) {
			t.Errorf("expected rejection of %q, got %v", name, err)
		}
	}
}

func TestValidateArchive_RejectsNotGzip(t *testing.T) {
	_, err := skills.ValidateArchive([]byte("not a gzip stream"))
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive, got %v", err)
	}
}

func newService(t *testing.T) (*skills.Service, *store.SkillStore, string) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	st := store.NewSkillStore(db)
	bundles := t.TempDir()
	return skills.New(st, bundles), st, bundles
}

func TestUpload_HappyPath(t *testing.T) {
	svc, st, bundles := newService(t)
	archive := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	res, err := svc.Upload(&skills.UploadInput{
		Version:    "1.0.0",
		Archive:    archive,
		Visibility: "public",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Skill.Slug != "test-skill" {
		t.Errorf("slug = %q", res.Skill.Slug)
	}
	if res.Skill.Visibility != "public" {
		t.Errorf("visibility = %q, want public", res.Skill.Visibility)
	}
	if res.Skill.LatestVersion != "1.0.0" {
		t.Errorf("LatestVersion = %q", res.Skill.LatestVersion)
	}
	if res.Version.ArchiveSize != int64(len(archive)) {
		t.Errorf("ArchiveSize mismatch")
	}
	want := sha256.Sum256(archive)
	if res.Version.ArchiveSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("ArchiveSHA256 mismatch")
	}

	on := filepath.Join(bundles, res.Version.ArchivePath)
	got, err := os.ReadFile(on)
	if err != nil {
		t.Fatalf("read archive on disk: %v", err)
	}
	if !bytes.Equal(got, archive) {
		t.Errorf("on-disk archive bytes differ from upload")
	}
	info, err := os.Stat(on)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("archive mode = %v, want 0600", mode)
	}

	// Skill row was created with manifest-derived display name.
	got2, _ := st.GetSkillBySlug("test-skill")
	if got2 == nil {
		t.Fatal("skill row missing after upload")
	}
}

func TestUpload_RecordsUploader(t *testing.T) {
	db := testutil.OpenTestDB(t)
	st := store.NewSkillStore(db)
	users := store.NewUserStore(db)
	svc := skills.New(st, t.TempDir())

	user, err := users.Create("uploader", "secret-pw", "admin")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	archive := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	res, err := svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: archive, UploadedBy: user.ID,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Version.UploadedBy == nil || *res.Version.UploadedBy != user.ID {
		t.Errorf("UploadedBy = %v, want %q", res.Version.UploadedBy, user.ID)
	}
	if res.Skill.CreatedBy == nil || *res.Skill.CreatedBy != user.ID {
		t.Errorf("CreatedBy = %v, want %q", res.Skill.CreatedBy, user.ID)
	}
}

func TestUpload_SecondVersionAdvancesLatest(t *testing.T) {
	svc, _, _ := newService(t)
	archive1 := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	if _, err := svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: archive1,
	}); err != nil {
		t.Fatalf("upload v1: %v", err)
	}
	archive2 := buildArchive(t, [][2]string{
		{"SKILL.md", strings.Replace(goodSkillMD, "A test skill", "Updated copy", 1)},
	})
	res, err := svc.Upload(&skills.UploadInput{
		Version: "1.1.0", Archive: archive2,
	})
	if err != nil {
		t.Fatalf("upload v2: %v", err)
	}
	if res.Skill.LatestVersion != "1.1.0" {
		t.Errorf("LatestVersion = %q, want 1.1.0", res.Skill.LatestVersion)
	}
}

func TestUpload_RejectsBadVersion(t *testing.T) {
	svc, _, _ := newService(t)
	archive := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	cases := []string{"v1.0.0", "1", "1.0", "latest", "1.0.0.0"}
	for _, v := range cases {
		_, err := svc.Upload(&skills.UploadInput{Version: v, Archive: archive})
		if !errors.Is(err, skills.ErrInvalidArchive) {
			t.Errorf("expected rejection of version %q, got %v", v, err)
		}
	}
}

func TestUpload_OversizeRejected(t *testing.T) {
	svc, _, _ := newService(t)
	big := bytes.Repeat([]byte{0}, skills.MaxArchiveSize+1)
	_, err := svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: big,
	})
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive, got %v", err)
	}
}

func TestUpload_SlugOverrideMustMatchManifest(t *testing.T) {
	svc, _, _ := newService(t)
	archive := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	_, err := svc.Upload(&skills.UploadInput{
		SlugOverride: "different-slug",
		Version:      "1.0.0",
		Archive:      archive,
	})
	if !errors.Is(err, skills.ErrInvalidArchive) {
		t.Fatalf("expected ErrInvalidArchive on slug mismatch, got %v", err)
	}
}

func TestOpenArchive_RoundTrip(t *testing.T) {
	svc, _, _ := newService(t)
	archive := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	res, err := svc.Upload(&skills.UploadInput{Version: "1.0.0", Archive: archive})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	rc, v, err := svc.OpenArchive(res.Skill.ID, "1.0.0")
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, archive) {
		t.Errorf("archive bytes mismatch on read")
	}
	if v.Version != "1.0.0" {
		t.Errorf("returned version metadata = %q", v.Version)
	}

	_, _, err = svc.OpenArchive(res.Skill.ID, "9.9.9")
	if !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("expected ErrSkillNotFound on missing version, got %v", err)
	}
}

func TestResolveLatest(t *testing.T) {
	svc, st, _ := newService(t)
	archive := buildArchive(t, [][2]string{{"SKILL.md", goodSkillMD}})

	if _, err := svc.Upload(&skills.UploadInput{Version: "1.0.0", Archive: archive}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	sk, v, err := svc.ResolveLatest("test-skill")
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if v.Version != "1.0.0" {
		t.Errorf("resolved version = %q", v.Version)
	}
	if sk.Slug != "test-skill" {
		t.Errorf("slug = %q", sk.Slug)
	}

	if err := st.YankSkill(sk.ID); err != nil {
		t.Fatalf("YankSkill: %v", err)
	}
	if _, _, err := svc.ResolveLatest("test-skill"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("expected ErrSkillNotFound after yank, got %v", err)
	}

	if _, _, err := svc.ResolveLatest("does-not-exist"); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("expected ErrSkillNotFound for missing slug, got %v", err)
	}
}
