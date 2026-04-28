package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/recipes"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// recipesBodyLimit caps create/update payload sizes. Recipes are pure JSON
// metadata (no archives), so the cap can be aggressive — anything above this
// is almost certainly malformed input.
const recipesBodyLimit = 64 * 1024 // 64 KiB

// RecipesHandlers wraps recipes.Service for HTTP. Like SkillsHandlers, it
// uses a closure to pull the authenticated user from context so the package
// stays free of an import-cycle dependency on internal/server.
type RecipesHandlers struct {
	svc         *recipes.Service
	store       *store.SetupRecipeStore
	userFromCtx func(context.Context) *store.User
}

// NewRecipesHandlers creates RecipesHandlers wired to a recipes.Service and
// the underlying store.
func NewRecipesHandlers(svc *recipes.Service, st *store.SetupRecipeStore, userFromCtx func(context.Context) *store.User) *RecipesHandlers {
	return &RecipesHandlers{svc: svc, store: st, userFromCtx: userFromCtx}
}

// HandleRecipes routes /api/recipes. GET = list-for-user, POST = create (admin).
func (h *RecipesHandlers) HandleRecipes(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.list(w, user)
	case http.MethodPost:
		if user.Role != "admin" {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		h.create(w, r, user.ID)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// HandleAssigned returns the user's effective recipe set: public + restricted-
// with-explicit-grant. This is what `arc-sync recipe sync` will consume.
func (h *RecipesHandlers) HandleAssigned(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := h.svc.AssignedForUser(user.ID)
	if err != nil {
		slog.Warn("recipes assigned", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assigned": rows})
}

// HandleRecipeByPath routes /api/recipes/{slug}.
func (h *RecipesHandlers) HandleRecipeByPath(w http.ResponseWriter, r *http.Request) {
	user := h.userFromCtx(r.Context())
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/recipes/")
	parts := strings.Split(rest, "/")
	slug := parts[0]
	if slug == "" || len(parts) > 1 {
		writeJSONError(w, http.StatusNotFound, "unknown subresource")
		return
	}
	recipe, err := h.store.GetRecipeBySlug(slug)
	if err != nil {
		slog.Warn("recipes lookup", "slug", slug, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if recipe == nil {
		writeJSONError(w, http.StatusNotFound, "recipe not found")
		return
	}

	// Read-side ACL: non-admins see public + their own assignments only.
	if r.Method == http.MethodGet && user.Role != "admin" {
		if !h.userCanRead(user, recipe) {
			writeJSONError(w, http.StatusNotFound, "recipe not found")
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, recipe)
	case http.MethodPatch:
		if user.Role != "admin" {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		h.update(w, r, recipe)
	case http.MethodDelete:
		if user.Role != "admin" {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		h.delete(w, r, recipe)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// listForUser returns recipes visible to the user: admins see all; non-admins
// see public + their own assignments. Yanked recipes are filtered for non-admins.
func (h *RecipesHandlers) list(w http.ResponseWriter, user *store.User) {
	var (
		out []*store.SetupRecipe
		err error
	)
	if user.Role == "admin" {
		out, err = h.store.ListRecipes()
	} else {
		var assigned []*store.AssignedRecipe
		assigned, err = h.store.AssignedForUser(user.ID)
		if err == nil {
			out = make([]*store.SetupRecipe, 0, len(assigned))
			for _, a := range assigned {
				out = append(out, a.Recipe)
			}
		}
	}
	if err != nil {
		slog.Warn("recipes list", "user", user.ID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recipes": out})
}

// createBody is the wire shape for POST /api/recipes.
type createBody struct {
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	RecipeType  string          `json:"recipe_type"`
	RecipeData  json.RawMessage `json:"recipe_data"`
	Visibility  string          `json:"visibility"`
}

func (h *RecipesHandlers) create(w http.ResponseWriter, r *http.Request, uploaderID string) {
	r.Body = http.MaxBytesReader(w, r.Body, recipesBodyLimit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body exceeds 64 KiB limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in createBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.svc.Create(&recipes.CreateInput{
		Slug:        in.Slug,
		DisplayName: in.DisplayName,
		Description: in.Description,
		RecipeType:  in.RecipeType,
		RecipeData:  in.RecipeData,
		Visibility:  in.Visibility,
		CreatedBy:   uploaderID,
	})
	if err != nil {
		switch {
		case errors.Is(err, recipes.ErrInvalidRecipe):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrRecipeSlugConflict):
			writeJSONError(w, http.StatusConflict, "slug already exists")
		default:
			slog.Warn("recipes create", "slug", in.Slug, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// updateBody is the wire shape for PATCH /api/recipes/{slug}.
type updateBody struct {
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Visibility  string          `json:"visibility"` // empty = preserve
	RecipeData  json.RawMessage `json:"recipe_data"` // empty = preserve
}

func (h *RecipesHandlers) update(w http.ResponseWriter, r *http.Request, recipe *store.SetupRecipe) {
	r.Body = http.MaxBytesReader(w, r.Body, recipesBodyLimit)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body exceeds 64 KiB limit")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	var in updateBody
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := h.svc.Update(recipe.ID, &recipes.UpdateInput{
		DisplayName: in.DisplayName,
		Description: in.Description,
		Visibility:  in.Visibility,
		RecipeData:  in.RecipeData,
	})
	if err != nil {
		switch {
		case errors.Is(err, recipes.ErrInvalidRecipe):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, recipes.ErrRecipeNotFound):
			writeJSONError(w, http.StatusNotFound, "recipe not found")
		default:
			slog.Warn("recipes update", "slug", recipe.Slug, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *RecipesHandlers) delete(w http.ResponseWriter, r *http.Request, recipe *store.SetupRecipe) {
	hard := r.URL.Query().Get("hard") == "true"
	if hard {
		if err := h.store.DeleteRecipe(recipe.ID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.store.YankRecipe(recipe.ID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yanked": true})
}

// userCanRead implements the visibility check for non-admin GETs. Mirrors
// AssignedForUser's WHERE clause: yanked hidden, public visible, restricted
// only with explicit assignment.
func (h *RecipesHandlers) userCanRead(user *store.User, recipe *store.SetupRecipe) bool {
	if recipe.YankedAt != nil {
		return false
	}
	if recipe.Visibility == "public" {
		return true
	}
	assigns, err := h.store.ListAssignmentsForRecipe(recipe.ID)
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
