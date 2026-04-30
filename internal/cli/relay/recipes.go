package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Recipe mirrors the relay's store.SetupRecipe JSON shape. Defined here (vs
// imported from internal/store) so arc-sync stays a CGO-free binary — same
// precedent as Skill and ingestRequest.
type Recipe struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	RecipeType  string          `json:"recipe_type"`
	RecipeData  json.RawMessage `json:"recipe_data"`
	Visibility  string          `json:"visibility"`
	YankedAt    *time.Time      `json:"yanked_at,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// AssignedRecipe is the row shape from GET /api/recipes/assigned.
type AssignedRecipe struct {
	Recipe *Recipe `json:"recipe"`
}

// ClaudePluginRecipeData is the recipe_data JSON shape for recipe_type ==
// "claude_plugin". Mirrored verbatim from internal/recipes.ClaudePluginRecipe
// — duplicated so arc-sync never imports the relay's recipes package.
type ClaudePluginRecipeData struct {
	MarketplaceSource string   `json:"marketplace_source"`
	Plugin            string   `json:"plugin"`
	Enabled           bool     `json:"enabled"`
	SparsePaths       []string `json:"sparse_paths,omitempty"`
}

// ListRecipes calls GET /api/recipes. Returns whatever the user can see.
func (c *Client) ListRecipes() ([]*Recipe, error) {
	body, err := c.recipeGet("/api/recipes")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Recipes []*Recipe `json:"recipes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse recipes list: %w", err)
	}
	return resp.Recipes, nil
}

// ListAssignedRecipes calls GET /api/recipes/assigned. Used by `arc-sync
// recipe sync` to compute the desired client state.
func (c *Client) ListAssignedRecipes() ([]*AssignedRecipe, error) {
	body, err := c.recipeGet("/api/recipes/assigned")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Assigned []*AssignedRecipe `json:"assigned"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse assigned recipes: %w", err)
	}
	return resp.Assigned, nil
}

// GetRecipe calls GET /api/recipes/{slug}. Returns nil with no error if the
// recipe doesn't exist or the user can't see it (HTTP 404).
func (c *Client) GetRecipe(slug string) (*Recipe, error) {
	body, err := c.recipeGet("/api/recipes/" + url.PathEscape(slug))
	if err != nil {
		if e, ok := err.(*recipeHTTPError); ok && e.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	var r Recipe
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse recipe: %w", err)
	}
	return &r, nil
}

// CreateRecipeRequest is the wire shape for POST /api/recipes.
type CreateRecipeRequest struct {
	Slug        string          `json:"slug"`
	DisplayName string          `json:"display_name,omitempty"`
	Description string          `json:"description,omitempty"`
	RecipeType  string          `json:"recipe_type"`
	RecipeData  json.RawMessage `json:"recipe_data"`
	Visibility  string          `json:"visibility,omitempty"`
}

// CreateRecipe posts to POST /api/recipes. Phase 4-style helper for `arc-sync
// recipe push`.
func (c *Client) CreateRecipe(req *CreateRecipeRequest) (*Recipe, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/recipes", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, handleErrorResponse(resp, respBody, fmt.Sprintf("recipe %q", req.Slug))
	}
	var out Recipe
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse create response: %w", err)
	}
	return &out, nil
}

// AssignRecipe grants a user access to a restricted recipe.
// POST /api/recipes/{slug}/assignments with body {username}.
// Idempotent: re-assigning replaces the prior assigned_by/at fields.
func (c *Client) AssignRecipe(slug, username string) error {
	body, err := json.Marshal(map[string]string{"username": username})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	endpoint := fmt.Sprintf("/api/recipes/%s/assignments", url.PathEscape(slug))
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return handleErrorResponse(resp, respBody, fmt.Sprintf("recipe %q assign %q", slug, username))
	}
	return nil
}

// UnassignRecipe revokes a user's grant.
// DELETE /api/recipes/{slug}/assignments/{username}.
func (c *Client) UnassignRecipe(slug, username string) error {
	endpoint := fmt.Sprintf("/api/recipes/%s/assignments/%s",
		url.PathEscape(slug), url.PathEscape(username))
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return handleErrorResponse(resp, body, fmt.Sprintf("recipe %q unassign %q", slug, username))
	}
	return nil
}

// YankRecipe calls DELETE /api/recipes/{slug}. Pass hard=true for true delete
// (admin only on the relay either way).
func (c *Client) YankRecipe(slug string, hard bool) error {
	endpoint := "/api/recipes/" + url.PathEscape(slug)
	if hard {
		endpoint += "?hard=true"
	}
	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+endpoint, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return handleErrorResponse(resp, body, fmt.Sprintf("recipe %q", slug))
	}
	return nil
}

// recipeHTTPError lets GetRecipe distinguish 404-not-found from network/auth
// errors — symmetric with SkillHTTPError.
type recipeHTTPError struct {
	Status int
	err    error
}

func (e *recipeHTTPError) Error() string { return e.err.Error() }
func (e *recipeHTTPError) Unwrap() error { return e.err }

func (c *Client) recipeGet(endpoint string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		err := handleErrorResponse(resp, body, "recipes")
		return nil, &recipeHTTPError{Status: resp.StatusCode, err: err}
	}
	return body, nil
}
