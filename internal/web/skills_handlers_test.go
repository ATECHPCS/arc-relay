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
