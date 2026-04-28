package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// SkillsHandlers wraps skills.Service for HTTP. Like MemoryHandlers, it uses a
// closure to pull the authenticated user from context — keeps the package free
// of an import-cycle dependency on internal/server. UserStore is used only to
// resolve username → user_id for the assignment endpoints.
type SkillsHandlers struct {
	svc         *skills.Service
	store       *store.SkillStore
	users       *store.UserStore
	userFromCtx func(context.Context) *store.User
}

// NewSkillsHandlers creates SkillsHandlers wired to the skills service +
// stores. userFromCtx returns nil for unauth'd callers; handlers fail closed
// in that case.
func NewSkillsHandlers(svc *skills.Service, st *store.SkillStore, users *store.UserStore, userFromCtx func(context.Context) *store.User) *SkillsHandlers {
	return &SkillsHandlers{svc: svc, store: st, users: users, userFromCtx: userFromCtx}
}

// HandleSkills routes /api/skills. GET = list-for-user, POST not allowed
// (uploads are versioned and routed through HandleSkillByPath).
func (h *SkillsHandlers) HandleSkills(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	skillsList, err := h.listForUser(user)
	if err != nil {
		slog.Warn("skills list", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": skillsList})
}

// HandleAssigned returns the user's effective skill set: public + restricted-
// with-explicit-grant, plus version pin (if any). This is what `arc-sync skill
// sync` consumes to compute the desired client state.
func (h *SkillsHandlers) HandleAssigned(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := h.store.AssignedForUser(user.ID)
	if err != nil {
		slog.Warn("skills assigned", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned": rows})
}

// HandleSkillByPath routes /api/skills/{slug}[/versions/{version}[/archive]].
// The leading prefix is stripped before this handler runs.
func (h *SkillsHandlers) HandleSkillByPath(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/skills/")
	parts := strings.Split(rest, "/")
	slug := parts[0]
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "missing skill slug")
		return
	}
	skill, err := h.store.GetSkillBySlug(slug)
	if err != nil {
		slog.Warn("skills lookup", "slug", slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Discoverability: GET on a non-existent slug returns 404. Non-admin write
	// callers also see 404 (don't leak existence to non-admins).
	if skill == nil {
		// For uploads (POST) we let the slug be created on the fly — a 404 here
		// would block the natural "publish a brand new skill" flow.
		if !(r.Method == http.MethodPost && len(parts) >= 3 && parts[1] == "versions") {
			writeJSONError(w, http.StatusNotFound, "skill not found")
			return
		}
	}

	// Read-side ACL for non-admins: they can see public + their own assignments.
	if r.Method == http.MethodGet && skill != nil && user.Role != "admin" {
		if !h.userCanRead(user, skill) {
			writeJSONError(w, http.StatusNotFound, "skill not found")
			return
		}
	}

	switch len(parts) {
	case 1:
		// /api/skills/{slug}
		switch r.Method {
		case http.MethodGet:
			h.getSkill(w, skill)
		case http.MethodDelete:
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			h.deleteSkill(w, r, skill)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case 2:
		// /api/skills/{slug}/versions   — list versions
		// /api/skills/{slug}/assignments — list assignments (admin) / POST grant (admin)
		switch parts[1] {
		case "versions":
			if r.Method != http.MethodGet {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.listVersions(w, skill)
		case "assignments":
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			switch r.Method {
			case http.MethodGet:
				h.listAssignments(w, skill)
			case http.MethodPost:
				h.assignSkill(w, r, skill, user.ID)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		default:
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
		}
	case 3:
		// /api/skills/{slug}/versions/{version}
		// /api/skills/{slug}/assignments/{username} — DELETE only (admin)
		switch parts[1] {
		case "versions":
			version := parts[2]
			if version == "" {
				writeJSONError(w, http.StatusBadRequest, "missing version")
				return
			}
			switch r.Method {
			case http.MethodGet:
				h.getVersion(w, skill, version)
			case http.MethodPost:
				if user.Role != "admin" {
					writeJSONError(w, http.StatusForbidden, "admin access required")
					return
				}
				h.uploadVersion(w, r, slug, version, user.ID)
			case http.MethodDelete:
				if user.Role != "admin" {
					writeJSONError(w, http.StatusForbidden, "admin access required")
					return
				}
				h.yankVersion(w, skill, version)
			default:
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
		case "assignments":
			username := parts[2]
			if username == "" {
				writeJSONError(w, http.StatusBadRequest, "missing username")
				return
			}
			if user.Role != "admin" {
				writeJSONError(w, http.StatusForbidden, "admin access required")
				return
			}
			if r.Method != http.MethodDelete {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.unassignSkill(w, skill, username)
		default:
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
		}
	case 4:
		// /api/skills/{slug}/versions/{version}/archive
		if parts[1] != "versions" || parts[3] != "archive" {
			writeJSONError(w, http.StatusNotFound, "unknown subresource")
			return
		}
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.downloadArchive(w, r, skill, parts[2])
	default:
		writeJSONError(w, http.StatusNotFound, "unknown subresource")
	}
}

// listForUser returns skills visible to the user: admins see all; non-admins
// see public + their own assignments. Yanked skills are filtered for non-admins.
func (h *SkillsHandlers) listForUser(user *store.User) ([]*store.Skill, error) {
	if user.Role == "admin" {
		return h.store.ListSkills()
	}
	rows, err := h.store.AssignedForUser(user.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Skill, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Skill)
	}
	return out, nil
}

// userCanRead implements the visibility check used by single-skill GET endpoints.
// Mirrors AssignedForUser's WHERE clause: public skills are readable by all
// authenticated users; restricted skills require an explicit assignment.
// Yanked skills are hidden from non-admins.
func (h *SkillsHandlers) userCanRead(user *store.User, skill *store.Skill) bool {
	if user.Role == "admin" {
		return true
	}
	if skill.YankedAt != nil {
		return false
	}
	if skill.Visibility == "public" {
		return true
	}
	// Restricted — check assignment table.
	assigns, err := h.store.ListAssignmentsForSkill(skill.ID)
	if err != nil {
		return false
	}
	for _, a := range assigns {
		if a.UserID == user.ID {
			return true
		}
	}
	return false
}

func (h *SkillsHandlers) getSkill(w http.ResponseWriter, skill *store.Skill) {
	versions, err := h.store.ListVersions(skill.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"skill":    skill,
		"versions": versions,
	})
}

func (h *SkillsHandlers) listVersions(w http.ResponseWriter, skill *store.Skill) {
	versions, err := h.store.ListVersions(skill.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (h *SkillsHandlers) getVersion(w http.ResponseWriter, skill *store.Skill, version string) {
	v, err := h.store.GetVersion(skill.ID, version)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if v == nil {
		writeJSONError(w, http.StatusNotFound, "version not found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *SkillsHandlers) uploadVersion(w http.ResponseWriter, r *http.Request, slug, version, uploaderID string) {
	r.Body = http.MaxBytesReader(w, r.Body, skills.MaxArchiveSize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "archive exceeds 5 MiB limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	visibility := r.URL.Query().Get("visibility")
	res, err := h.svc.Upload(&skills.UploadInput{
		SlugOverride: slug,
		Version:      version,
		Archive:      body,
		UploadedBy:   uploaderID,
		Visibility:   visibility,
	})
	if err != nil {
		switch {
		case errors.Is(err, skills.ErrInvalidArchive):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrSkillVersionConflict):
			writeJSONError(w, http.StatusConflict, "version already exists")
		case errors.Is(err, store.ErrSkillSlugConflict):
			writeJSONError(w, http.StatusConflict, "slug already exists")
		default:
			slog.Warn("skills upload", "slug", slug, "version", version, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *SkillsHandlers) deleteSkill(w http.ResponseWriter, r *http.Request, skill *store.Skill) {
	hard := r.URL.Query().Get("hard") == "true"
	if hard {
		if err := h.store.DeleteSkill(skill.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.YankSkill(skill.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yanked": true})
}

func (h *SkillsHandlers) yankVersion(w http.ResponseWriter, skill *store.Skill, version string) {
	if err := h.store.YankVersion(skill.ID, version); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yanked": true})
}

// downloadArchive streams the archive bytes back to the client. We do not set
// Content-Length here; ServeContent would be wrong because we want the strong
// SHA-256 hash in headers and a binary download disposition.
func (h *SkillsHandlers) downloadArchive(w http.ResponseWriter, _ *http.Request, skill *store.Skill, version string) {
	rc, v, err := h.svc.OpenArchive(skill.ID, version)
	if err != nil {
		if errors.Is(err, skills.ErrSkillNotFound) {
			writeJSONError(w, http.StatusNotFound, "version not found")
			return
		}
		slog.Warn("skills download", "slug", skill.Slug, "version", version, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+skill.Slug+`-`+v.Version+`.tar.gz"`)
	w.Header().Set("X-Skill-SHA256", v.ArchiveSHA256)
	w.Header().Set("X-Skill-Version", v.Version)
	if _, err := io.Copy(w, rc); err != nil {
		// Already wrote headers — best we can do is stop streaming. Don't try
		// to write a JSON error body; that races the response writer.
		slog.Warn("skills download stream error", "slug", skill.Slug, "version", version, "err", err)
	}
}

// writeJSONError writes a {"error":msg} body with the given status. Reuses
// writeJSON from handlers.go.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// assignBody is the wire shape for POST /api/skills/{slug}/assignments and
// the analogous recipes endpoint. Username is resolved server-side to user_id;
// version is optional (NULL means "follow latest").
type assignBody struct {
	Username string `json:"username"`
	Version  string `json:"version,omitempty"`
}

// listAssignments returns the existing grants for a skill. Admin-only at the
// route level — all callers reaching here have already passed the admin check.
func (h *SkillsHandlers) listAssignments(w http.ResponseWriter, skill *store.Skill) {
	rows, err := h.store.ListAssignmentsForSkill(skill.ID)
	if err != nil {
		slog.Warn("skills list assignments", "slug", skill.Slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": rows})
}

// assignSkill grants a user access to a restricted skill. Body shape:
//   {"username":"alice","version":"1.0.0"}
// version is optional. Idempotent: re-assigning replaces the prior pin.
func (h *SkillsHandlers) assignSkill(w http.ResponseWriter, r *http.Request, skill *store.Skill, adminID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in assignBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(in.Username) == "" {
		writeJSONError(w, http.StatusBadRequest, "username is required")
		return
	}
	target, err := h.users.GetByUsername(in.Username)
	if err != nil {
		slog.Warn("skills assign user lookup", "username", in.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	a := &store.SkillAssignment{
		SkillID: skill.ID,
		UserID:  target.ID,
	}
	if v := strings.TrimSpace(in.Version); v != "" {
		a.Version = &v
	}
	if adminID != "" {
		a.AssignedBy = &adminID
	}
	if err := h.store.AssignSkill(a); err != nil {
		slog.Warn("skills assign", "slug", skill.Slug, "user", in.Username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// unassignSkill revokes a grant. The skill_id + username must both resolve
// for idempotency to be useful: an unassign on a non-existent user returns
// 404 so the caller knows the typo wasn't accepted as a no-op.
func (h *SkillsHandlers) unassignSkill(w http.ResponseWriter, skill *store.Skill, username string) {
	target, err := h.users.GetByUsername(username)
	if err != nil {
		slog.Warn("skills unassign user lookup", "username", username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if err := h.store.UnassignSkill(skill.ID, target.ID); err != nil {
		slog.Warn("skills unassign", "slug", skill.Slug, "user", username, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
