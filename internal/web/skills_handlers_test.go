package web_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
	"github.com/comma-compliance/arc-relay/internal/web"
)

const skillMD = `---
name: demo-skill
description: Demo skill for handler tests.
user-invocable: true
---

# Demo
`

// skillsRig wires up SkillsHandlers + a test mux that lets each subtest pick
// the user injected into context. Returns the mux and the underlying skill
// store so tests can seed data without hitting the upload path.
type skillsRig struct {
	mux          *http.ServeMux
	store        *store.SkillStore
	svc          *skills.Service
	users        *store.UserStore
	admin        *store.User
	userToInject *store.User
}

func newSkillsRig(t *testing.T) *skillsRig {
	t.Helper()
	db := testutil.OpenTestDB(t)
	st := store.NewSkillStore(db)
	svc := skills.New(st, t.TempDir())
	users := store.NewUserStore(db)

	// Seed an admin user so audit FKs (skills.created_by, skill_versions.uploaded_by)
	// can resolve. Without this, every upload trips ON DELETE SET NULL → FK violation.
	admin, err := users.Create("test-admin", "test-pw", "admin")
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := web.NewSkillsHandlers(svc, st, users, func(ctx context.Context) *store.User {
		return server.UserFromContext(ctx)
	})

	rig := &skillsRig{store: st, svc: svc, users: users, admin: admin, mux: http.NewServeMux()}
	wrap := func(handler http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if rig.userToInject != nil {
				ctx = server.WithUser(ctx, rig.userToInject)
			}
			handler(w, r.WithContext(ctx))
		})
	}
	rig.mux.Handle("/api/skills", wrap(h.HandleSkills))
	rig.mux.Handle("/api/skills/assigned", wrap(h.HandleAssigned))
	rig.mux.Handle("/api/skills/", wrap(h.HandleSkillByPath))
	return rig
}

// regularUser builds a fake non-admin user, with an existing DB row so FKs
// resolve when the user uploads.
func (r *skillsRig) regularUser(t *testing.T, username string) *store.User {
	t.Helper()
	u, err := r.users.Create(username, "test-pw", "user")
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

// makeArchive builds a minimal gzipped tar with one SKILL.md at root.
func makeArchive(t *testing.T, frontmatter string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "SKILL.md", Mode: 0o644, Size: int64(len(frontmatter)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte(frontmatter)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestSkillsHandlers_RequiresAuth(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = nil

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills", nil))
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/skills unauth = %d, want 401", rw.Code)
	}
}

func TestSkillsHandlers_UploadAdminOnly(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.regularUser(t, "ian")

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0",
		bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/gzip")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin upload = %d, want 403; body=%s", rw.Code, rw.Body.String())
	}
}

func TestSkillsHandlers_UploadHappyPath(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0?visibility=public",
		bytes.NewReader(archive))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusCreated {
		t.Fatalf("upload = %d, body=%s", rw.Code, rw.Body.String())
	}
	var res skills.UploadResult
	if err := json.Unmarshal(rw.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Skill.Slug != "demo-skill" {
		t.Errorf("slug = %q", res.Skill.Slug)
	}
	if res.Skill.Visibility != "public" {
		t.Errorf("visibility = %q", res.Skill.Visibility)
	}
	if res.Version.Version != "1.0.0" {
		t.Errorf("version = %q", res.Version.Version)
	}
}

func TestSkillsHandlers_UploadOversize(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	big := bytes.Repeat([]byte{0}, skills.MaxArchiveSize+10)
	req := httptest.NewRequest("POST", "/api/skills/big-skill/versions/1.0.0",
		bytes.NewReader(big))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload = %d, want 413; body=%s", rw.Code, rw.Body.String())
	}
}

func TestSkillsHandlers_UploadDuplicateVersion(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0",
		bytes.NewReader(archive))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("first upload = %d", rw.Code)
	}

	req = httptest.NewRequest("POST", "/api/skills/demo-skill/versions/1.0.0",
		bytes.NewReader(archive))
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusConflict {
		t.Errorf("duplicate upload = %d, want 409", rw.Code)
	}
}

func TestSkillsHandlers_GetSkill(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Seed via service.
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET = %d", rw.Code)
	}
	var resp struct {
		Skill    *store.Skill          `json:"skill"`
		Versions []*store.SkillVersion `json:"versions"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Skill.Slug != "demo-skill" {
		t.Errorf("slug = %q", resp.Skill.Slug)
	}
	if len(resp.Versions) != 1 {
		t.Errorf("versions len = %d", len(resp.Versions))
	}
}

func TestSkillsHandlers_DownloadArchive_RoundTrip(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	original := makeArchive(t, skillMD)
	res, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: original, Visibility: "public",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET",
		"/api/skills/demo-skill/versions/1.0.0/archive", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("download = %d body=%s", rw.Code, rw.Body.String())
	}
	if !bytes.Equal(rw.Body.Bytes(), original) {
		t.Errorf("downloaded bytes differ from upload")
	}
	if got := rw.Header().Get("X-Skill-SHA256"); got != res.Version.ArchiveSHA256 {
		t.Errorf("X-Skill-SHA256 = %q, want %q", got, res.Version.ArchiveSHA256)
	}
	if got := rw.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestSkillsHandlers_AssignedFiltersByVisibility(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("upload public: %v", err)
	}
	secretMD := strings.Replace(skillMD, "demo-skill", "secret-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, secretMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("upload restricted: %v", err)
	}

	// Switch to a regular user; they should see only the public skill.
	rig.userToInject = rig.regularUser(t, "user7")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("assigned = %d body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Assigned []*store.AssignedSkill `json:"assigned"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Assigned) != 1 || resp.Assigned[0].Skill.Slug != "demo-skill" {
		t.Fatalf("expected only demo-skill, got %+v", resp.Assigned)
	}
}

func TestSkillsHandlers_RegularUserCannotSeeRestrictedDirectly(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	secretMD := strings.Replace(skillMD, "demo-skill", "secret-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, secretMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rig.userToInject = rig.regularUser(t, "outsider")
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/secret-skill", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("non-admin GET on restricted = %d, want 404", rw.Code)
	}
}

func TestSkillsHandlers_AdminListSeesAll(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("upload public: %v", err)
	}
	secretMD := strings.Replace(skillMD, "demo-skill", "secret-skill", 1)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, secretMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("upload restricted: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list = %d body=%s", rw.Code, rw.Body.String())
	}
	var resp struct {
		Skills []*store.Skill `json:"skills"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Skills) != 2 {
		t.Errorf("admin list len = %d, want 2", len(resp.Skills))
	}
}

func TestSkillsHandlers_YankSkill(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "public",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("yank = %d body=%s", rw.Code, rw.Body.String())
	}

	got, _ := rig.store.GetSkillBySlug("demo-skill")
	if got == nil || got.YankedAt == nil {
		t.Errorf("skill should be present and yanked, got %+v", got)
	}

	// Hard delete removes it.
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw,
		httptest.NewRequest("DELETE", "/api/skills/demo-skill?hard=true", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("hard delete = %d", rw.Code)
	}
	got, _ = rig.store.GetSkillBySlug("demo-skill")
	if got != nil {
		t.Errorf("skill should be deleted, got %+v", got)
	}
}

func TestSkillsHandlers_YankVersion(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD),
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw,
		httptest.NewRequest("DELETE", "/api/skills/demo-skill/versions/1.0.0", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("yank version = %d body=%s", rw.Code, rw.Body.String())
	}
	sk, _ := rig.store.GetSkillBySlug("demo-skill")
	v, _ := rig.store.GetVersion(sk.ID, "1.0.0")
	if v == nil || v.YankedAt == nil {
		t.Errorf("version should be yanked, got %+v", v)
	}
}

func TestSkillsHandlers_BadVersionFormat(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	archive := makeArchive(t, skillMD)
	req := httptest.NewRequest("POST", "/api/skills/demo-skill/versions/latest",
		bytes.NewReader(archive))
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("bad-version upload = %d, want 400; body=%s", rw.Code, rw.Body.String())
	}
}

func TestSkillsHandlers_AssignedRouteNotShadowedByPath(t *testing.T) {
	// Regression: ensure /api/skills/assigned hits HandleAssigned, not
	// HandleSkillByPath, even though both prefixes overlap. The ServeMux
	// resolves longer-pattern wins, but only because we register both.
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/assigned", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("assigned = %d body=%s", rw.Code, rw.Body.String())
	}
	// Body should be {"assigned": [...]}, NOT a "skill not found" response from
	// HandleSkillByPath treating "assigned" as a slug.
	if !bytes.Contains(rw.Body.Bytes(), []byte(`"assigned":`)) {
		t.Errorf("response body did not match HandleAssigned shape: %s", rw.Body.String())
	}
}

// readGzipFirstFile is a small helper used to peek into the downloaded archive
// and confirm the body really is the gzipped tar we uploaded.
func readGzipFirstFile(t *testing.T, b []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	contents, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("tar read: %v", err)
	}
	return hdr.Name + ":" + string(contents)
}

func TestSkillsHandlers_AssignmentLifecycle(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Seed a restricted skill so visibility-gated reads matter.
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	alice := rig.regularUser(t, "alice")

	// Pre-grant: alice cannot see the restricted skill.
	rig.userToInject = alice
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("pre-grant non-admin GET = %d, want 404", rw.Code)
	}

	// Admin grants alice access (with a version pin).
	rig.userToInject = rig.admin
	body := `{"username":"alice","version":"1.0.0"}`
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments", strings.NewReader(body)))
	if rw.Code != http.StatusCreated {
		t.Fatalf("assign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Post-grant: alice now sees it.
	rig.userToInject = alice
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusOK {
		t.Errorf("post-grant non-admin GET = %d, want 200", rw.Code)
	}

	// Admin lists assignments and sees alice.
	rig.userToInject = rig.admin
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill/assignments", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("list assignments = %d", rw.Code)
	}
	if !bytes.Contains(rw.Body.Bytes(), []byte(`"user_id":"`+alice.ID+`"`)) {
		t.Errorf("list missing alice: %s", rw.Body.String())
	}

	// Re-assign with a different version (should upsert, not error).
	body2 := `{"username":"alice","version":"2.0.0"}`
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments", strings.NewReader(body2)))
	if rw.Code != http.StatusCreated {
		t.Fatalf("re-assign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Unassign.
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/skills/demo-skill/assignments/alice", nil))
	if rw.Code != http.StatusNoContent {
		t.Fatalf("unassign = %d body=%s", rw.Code, rw.Body.String())
	}

	// Post-unassign: alice no longer sees the skill.
	rig.userToInject = alice
	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET", "/api/skills/demo-skill", nil))
	if rw.Code != http.StatusNotFound {
		t.Errorf("post-unassign non-admin GET = %d, want 404", rw.Code)
	}
}

func TestSkillsHandlers_AssignNonAdminForbidden(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rig.regularUser(t, "alice")
	rig.userToInject = rig.regularUser(t, "bob")

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments",
		strings.NewReader(`{"username":"alice"}`)))
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin assign = %d, want 403", rw.Code)
	}

	rw = httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("DELETE", "/api/skills/demo-skill/assignments/alice", nil))
	if rw.Code != http.StatusForbidden {
		t.Errorf("non-admin unassign = %d, want 403", rw.Code)
	}
}

func TestSkillsHandlers_AssignRejectsUnknownUser(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: makeArchive(t, skillMD), Visibility: "restricted",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("POST", "/api/skills/demo-skill/assignments",
		strings.NewReader(`{"username":"nobody-such-user"}`)))
	if rw.Code != http.StatusNotFound {
		t.Errorf("assign unknown user = %d, want 404", rw.Code)
	}
}

func TestSkillsHandlers_DownloadArchiveContents(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin
	original := makeArchive(t, skillMD)
	if _, err := rig.svc.Upload(&skills.UploadInput{
		Version: "1.0.0", Archive: original, Visibility: "public",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}

	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, httptest.NewRequest("GET",
		"/api/skills/demo-skill/versions/1.0.0/archive", nil))
	got := readGzipFirstFile(t, rw.Body.Bytes())
	if !strings.HasPrefix(got, "SKILL.md:") {
		t.Errorf("archive first file = %q", got)
	}
}

// uploadResp matches the wire shape returned by uploadVersion: skills.UploadResult
// embedded plus an upstream_recorded bool added by the handler.
type uploadResp struct {
	skills.UploadResult
	UpstreamRecorded bool `json:"upstream_recorded"`
}

// pushVersion is a small helper for the upstream-related tests: builds an
// archive for `slug` at `version`, applies any caller-supplied headers, and
// returns the parsed response after asserting 201.
func pushVersion(t *testing.T, rig *skillsRig, slug, version, frontmatter string, headers map[string]string) *uploadResp {
	t.Helper()
	md := frontmatter
	if md == "" {
		md = strings.Replace(skillMD, "demo-skill", slug, 1)
	}
	archive := makeArchive(t, md)
	req := httptest.NewRequest("POST",
		"/api/skills/"+slug+"/versions/"+version+"?visibility=public",
		bytes.NewReader(archive))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rw := httptest.NewRecorder()
	rig.mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusCreated {
		t.Fatalf("push %s@%s = %d body=%s", slug, version, rw.Code, rw.Body.String())
	}
	var resp uploadResp
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &resp
}

// TestSkillsHandlers_UploadWithUpstreamHeader: push with X-Upstream creates a
// new upstream row and reports upstream_recorded=true.
func TestSkillsHandlers_UploadWithUpstreamHeader(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	resp := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":"https://github.com/example/repo","subpath":"skills/demo","ref":"main"}`,
	})
	if !resp.UpstreamRecorded {
		t.Fatalf("upstream_recorded=false, want true; resp=%+v", resp)
	}

	u, err := rig.store.GetUpstream(resp.Skill.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u == nil {
		t.Fatal("expected upstream row, got nil")
	}
	if u.GitURL != "https://github.com/example/repo" {
		t.Errorf("GitURL = %q", u.GitURL)
	}
	if u.GitSubpath != "skills/demo" {
		t.Errorf("GitSubpath = %q", u.GitSubpath)
	}
	if u.GitRef != "main" {
		t.Errorf("GitRef = %q", u.GitRef)
	}
	if u.UpstreamType != "git" {
		t.Errorf("UpstreamType = %q", u.UpstreamType)
	}
}

// TestSkillsHandlers_UploadPreservesExistingUpstreamAndClearsDrift: push without
// any upstream header leaves an existing row in place AND clears drift fields.
func TestSkillsHandlers_UploadPreservesExistingUpstreamAndClearsDrift(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// First push with metadata to create the row.
	first := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":"https://github.com/example/repo","ref":"main"}`,
	})
	skillID := first.Skill.ID

	// Seed a drift report so we can assert it gets cleared.
	if err := rig.store.WriteDriftReport(skillID, &store.DriftReport{
		RelayVersion:      "1.0.0",
		RelayHash:         "relayhash",
		UpstreamSHA:       "abc",
		UpstreamHash:      "upstreamhash",
		CommitsAhead:      2,
		Severity:          "minor",
		Summary:           "minor change upstream",
		RecommendedAction: "consider pulling",
		LLMModel:          "test",
		DetectedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	// Confirm drift was actually persisted before the next push.
	pre, err := rig.store.GetUpstream(skillID)
	if err != nil || pre == nil {
		t.Fatalf("GetUpstream pre: u=%v err=%v", pre, err)
	}
	if pre.DriftDetectedAt == nil {
		t.Fatal("expected DriftDetectedAt to be set after WriteDriftReport")
	}

	// Second push with NO upstream headers. Row should survive; drift should clear.
	resp := pushVersion(t, rig, "demo-skill", "1.1.0", "", nil)
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on no-metadata push, want false")
	}

	post, err := rig.store.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream post: %v", err)
	}
	if post == nil {
		t.Fatal("upstream row was deleted by no-metadata push, want preserved")
	}
	if post.GitURL != "https://github.com/example/repo" {
		t.Errorf("GitURL changed: %q", post.GitURL)
	}
	if post.DriftDetectedAt != nil {
		t.Errorf("DriftDetectedAt should be nil after clear, got %v", post.DriftDetectedAt)
	}
	if post.DriftSeverity != nil {
		t.Errorf("DriftSeverity should be nil after clear, got %v", post.DriftSeverity)
	}
}

// TestSkillsHandlers_UploadClearUpstreamHeader: push with X-Clear-Upstream:true
// deletes the existing upstream row and reports upstream_recorded=false.
func TestSkillsHandlers_UploadClearUpstreamHeader(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Seed a row.
	first := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":"https://github.com/example/repo"}`,
	})
	skillID := first.Skill.ID

	resp := pushVersion(t, rig, "demo-skill", "1.1.0", "", map[string]string{
		"X-Clear-Upstream": "true",
	})
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on clear, want false")
	}

	u, err := rig.store.GetUpstream(skillID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("upstream row should be deleted after clear, got %+v", u)
	}
}

// TestSkillsHandlers_UploadNoMetadataNoRow: push with no upstream metadata and
// no existing row → no-op on upstream side.
func TestSkillsHandlers_UploadNoMetadataNoRow(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	resp := pushVersion(t, rig, "demo-skill", "1.0.0", "", nil)
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true with no metadata, want false")
	}

	u, err := rig.store.GetUpstream(resp.Skill.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("expected no upstream row, got %+v", u)
	}
}

// TestSkillsHandlers_UploadMalformedUpstreamHeader: malformed JSON or wrong
// type doesn't crash the handler and doesn't write a row; upstream_recorded=false.
func TestSkillsHandlers_UploadMalformedUpstreamHeader(t *testing.T) {
	rig := newSkillsRig(t)
	rig.userToInject = rig.admin

	// Malformed JSON.
	resp := pushVersion(t, rig, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `not-json`,
	})
	if resp.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on malformed JSON, want false")
	}
	u, err := rig.store.GetUpstream(resp.Skill.ID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if u != nil {
		t.Errorf("malformed JSON should not write a row, got %+v", u)
	}

	// Wrong type.
	rigB := newSkillsRig(t)
	rigB.userToInject = rigB.admin
	respB := pushVersion(t, rigB, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"svn","url":"https://example.com/repo"}`,
	})
	if respB.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on type=svn, want false")
	}

	// Empty URL.
	rigC := newSkillsRig(t)
	rigC.userToInject = rigC.admin
	respC := pushVersion(t, rigC, "demo-skill", "1.0.0", "", map[string]string{
		"X-Upstream": `{"type":"git","url":""}`,
	})
	if respC.UpstreamRecorded {
		t.Errorf("upstream_recorded=true on empty url, want false")
	}
}
