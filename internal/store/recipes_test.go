package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

func TestRecipeCreate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)

	r := &store.SetupRecipe{
		Slug:        "claude-mem",
		DisplayName: "Claude Mem",
		Description: "Memory plugin",
		RecipeType:  store.RecipeTypeClaudePlugin,
		RecipeData:  json.RawMessage(`{"marketplace_source":"thedotmack/claude-mem","plugin":"claude-mem@thedotmack","enabled":true}`),
	}
	if err := recipes.CreateRecipe(r); err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}
	if r.ID == "" {
		t.Error("ID should be generated")
	}
	if r.Visibility != "restricted" {
		t.Errorf("default visibility = %q", r.Visibility)
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		t.Error("timestamps should be set")
	}
}

func TestRecipeCreate_RejectsBadSlug(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	cases := []string{"-bad", "BAD", "ab--", "a", "", "with:colon"}
	for _, slug := range cases {
		r := &store.SetupRecipe{
			Slug: slug, DisplayName: "x",
			RecipeType: store.RecipeTypeClaudePlugin,
			RecipeData: json.RawMessage(`{}`),
		}
		if err := recipes.CreateRecipe(r); err == nil {
			t.Errorf("CreateRecipe(%q) accepted invalid slug", slug)
		}
	}
}

func TestRecipeCreate_RejectsUnknownRecipeType(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	r := &store.SetupRecipe{
		Slug: "shellish", DisplayName: "x",
		RecipeType: "shell_script",
		RecipeData: json.RawMessage(`{"script":"echo hi"}`),
	}
	if err := recipes.CreateRecipe(r); err == nil {
		t.Fatal("expected CHECK constraint failure for unknown recipe_type")
	}
}

func TestRecipeCreate_DuplicateSlug(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	first := &store.SetupRecipe{
		Slug: "twin", DisplayName: "First",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	}
	if err := recipes.CreateRecipe(first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := &store.SetupRecipe{
		Slug: "twin", DisplayName: "Second",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"c/d","plugin":"y"}`),
	}
	err := recipes.CreateRecipe(second)
	if !errors.Is(err, store.ErrRecipeSlugConflict) {
		t.Fatalf("expected ErrRecipeSlugConflict, got %v", err)
	}
}

func TestRecipeGetByIDAndSlug(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	r := &store.SetupRecipe{
		Slug: "lookup", DisplayName: "Lookup", Visibility: "public",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	}
	if err := recipes.CreateRecipe(r); err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}
	got, err := recipes.GetRecipe(r.ID)
	if err != nil {
		t.Fatalf("GetRecipe: %v", err)
	}
	if got == nil || got.Slug != "lookup" {
		t.Fatalf("GetRecipe = %+v", got)
	}
	bySlug, err := recipes.GetRecipeBySlug("lookup")
	if err != nil {
		t.Fatalf("GetRecipeBySlug: %v", err)
	}
	if bySlug == nil || bySlug.ID != r.ID {
		t.Fatalf("GetRecipeBySlug = %+v", bySlug)
	}
	missing, _ := recipes.GetRecipeBySlug("nope")
	if missing != nil {
		t.Fatalf("expected nil for missing slug, got %+v", missing)
	}
}

func TestRecipeUpdate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	r := &store.SetupRecipe{
		Slug: "patch", DisplayName: "Old",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	}
	if err := recipes.CreateRecipe(r); err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}
	newData := json.RawMessage(`{"marketplace_source":"c/d","plugin":"y","enabled":false}`)
	if err := recipes.UpdateRecipe(r.ID, "New", "New desc", "public", newData); err != nil {
		t.Fatalf("UpdateRecipe: %v", err)
	}
	got, _ := recipes.GetRecipe(r.ID)
	if got.DisplayName != "New" || got.Description != "New desc" || got.Visibility != "public" {
		t.Errorf("UpdateRecipe did not apply: %+v", got)
	}
	if string(got.RecipeData) != string(newData) {
		t.Errorf("recipe_data not updated: %s", got.RecipeData)
	}
	// Empty visibility must NOT clear the field.
	if err := recipes.UpdateRecipe(r.ID, "Newer", "still", "", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("UpdateRecipe empty vis: %v", err)
	}
	got, _ = recipes.GetRecipe(r.ID)
	if got.Visibility != "public" {
		t.Errorf("Visibility cleared on empty input: %+v", got)
	}
}

func TestRecipeYankUnyank(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	r := &store.SetupRecipe{
		Slug: "yankee", DisplayName: "x",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	}
	if err := recipes.CreateRecipe(r); err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}
	if err := recipes.YankRecipe(r.ID); err != nil {
		t.Fatalf("YankRecipe: %v", err)
	}
	got, _ := recipes.GetRecipe(r.ID)
	if got.YankedAt == nil {
		t.Error("YankedAt should be set after YankRecipe")
	}
	if err := recipes.UnyankRecipe(r.ID); err != nil {
		t.Fatalf("UnyankRecipe: %v", err)
	}
	got, _ = recipes.GetRecipe(r.ID)
	if got.YankedAt != nil {
		t.Error("YankedAt should be nil after UnyankRecipe")
	}
}

func TestRecipeAssignAndAssignedForUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	recipes := store.NewSetupRecipeStore(db)
	users := store.NewUserStore(db)

	user, err := users.Create("alice", "secret-pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	pub := &store.SetupRecipe{
		Slug: "shared-recipe", DisplayName: "Shared", Visibility: "public",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"a/b","plugin":"x"}`),
	}
	if err := recipes.CreateRecipe(pub); err != nil {
		t.Fatalf("CreateRecipe public: %v", err)
	}
	rest := &store.SetupRecipe{
		Slug: "team-recipe", DisplayName: "Team", Visibility: "restricted",
		RecipeType: store.RecipeTypeClaudePlugin,
		RecipeData: json.RawMessage(`{"marketplace_source":"c/d","plugin":"y"}`),
	}
	if err := recipes.CreateRecipe(rest); err != nil {
		t.Fatalf("CreateRecipe restricted: %v", err)
	}

	// Alice initially sees only the public recipe.
	assigned, err := recipes.AssignedForUser(user.ID)
	if err != nil {
		t.Fatalf("AssignedForUser pre: %v", err)
	}
	if len(assigned) != 1 || assigned[0].Recipe.Slug != "shared-recipe" {
		t.Fatalf("expected only shared-recipe, got %+v", assigned)
	}

	// Grant restricted; now alice sees both.
	if err := recipes.AssignRecipe(&store.SetupRecipeAssignment{
		RecipeID: rest.ID, UserID: user.ID,
	}); err != nil {
		t.Fatalf("AssignRecipe: %v", err)
	}
	assigned, _ = recipes.AssignedForUser(user.ID)
	if len(assigned) != 2 {
		t.Fatalf("expected 2 assigned, got %d", len(assigned))
	}

	// Yanking the public recipe removes it from listings.
	if err := recipes.YankRecipe(pub.ID); err != nil {
		t.Fatalf("YankRecipe: %v", err)
	}
	assigned, _ = recipes.AssignedForUser(user.ID)
	if len(assigned) != 1 || assigned[0].Recipe.Slug != "team-recipe" {
		t.Errorf("expected only team-recipe after yank, got %+v", assigned)
	}

	// Unassign drops alice back to public-only (empty after yank above).
	if err := recipes.UnassignRecipe(rest.ID, user.ID); err != nil {
		t.Fatalf("UnassignRecipe: %v", err)
	}
	assigned, _ = recipes.AssignedForUser(user.ID)
	if len(assigned) != 0 {
		t.Fatalf("expected 0 after unassign+yank, got %d", len(assigned))
	}
}
