package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

func TestSkillCreate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{
		Slug:        "demo-skill",
		DisplayName: "Demo Skill",
		Description: "Does demo things",
	}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	if sk.ID == "" {
		t.Error("ID should be generated")
	}
	if sk.Visibility != "restricted" {
		t.Errorf("default visibility = %q, want restricted", sk.Visibility)
	}
	if sk.CreatedAt.IsZero() || sk.UpdatedAt.IsZero() {
		t.Error("timestamps should be set")
	}
}

func TestSkillCreate_RejectsBadSlug(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	cases := []string{"-bad", "BAD", "ab--", "a", ""}
	for _, slug := range cases {
		sk := &store.Skill{Slug: slug, DisplayName: "x"}
		if err := skills.CreateSkill(sk); err == nil {
			t.Errorf("CreateSkill(%q) accepted invalid slug", slug)
		}
	}
}

func TestSkillCreate_DuplicateSlug(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	first := &store.Skill{Slug: "twin", DisplayName: "First"}
	if err := skills.CreateSkill(first); err != nil {
		t.Fatalf("first CreateSkill: %v", err)
	}
	second := &store.Skill{Slug: "twin", DisplayName: "Second"}
	err := skills.CreateSkill(second)
	if !errors.Is(err, store.ErrSkillSlugConflict) {
		t.Fatalf("expected ErrSkillSlugConflict, got %v", err)
	}
}

func TestSkillVisibilityValidated(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{Slug: "vis-skill", DisplayName: "x", Visibility: "secret"}
	if err := skills.CreateSkill(sk); err == nil {
		t.Fatal("CreateSkill accepted invalid visibility")
	}
}

func TestSkillGetByIDAndSlug(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{Slug: "lookup", DisplayName: "Lookup", Visibility: "public"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	got, err := skills.GetSkill(sk.ID)
	if err != nil {
		t.Fatalf("GetSkill: %v", err)
	}
	if got == nil || got.Slug != "lookup" {
		t.Fatalf("GetSkill returned %+v", got)
	}
	bySlug, err := skills.GetSkillBySlug("lookup")
	if err != nil {
		t.Fatalf("GetSkillBySlug: %v", err)
	}
	if bySlug == nil || bySlug.ID != sk.ID {
		t.Fatalf("GetSkillBySlug returned %+v", bySlug)
	}
	missing, err := skills.GetSkillBySlug("nope")
	if err != nil {
		t.Fatalf("GetSkillBySlug missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("GetSkillBySlug missing returned %+v, want nil", missing)
	}
}

func TestSkillCreateVersionAdvancesLatest(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{Slug: "ver-skill", DisplayName: "Ver"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	v1 := &store.SkillVersion{
		SkillID:       sk.ID,
		Version:       "1.0.0",
		ArchivePath:   "ver-skill/1.0.0.tar.gz",
		ArchiveSize:   1234,
		ArchiveSHA256: "deadbeef",
		Manifest:      json.RawMessage(`{"name":"ver-skill"}`),
	}
	if err := skills.CreateVersion(v1); err != nil {
		t.Fatalf("CreateVersion v1: %v", err)
	}
	got, err := skills.GetSkill(sk.ID)
	if err != nil {
		t.Fatalf("GetSkill after v1: %v", err)
	}
	if got.LatestVersion != "1.0.0" {
		t.Errorf("LatestVersion = %q, want 1.0.0", got.LatestVersion)
	}

	v2 := &store.SkillVersion{
		SkillID:       sk.ID,
		Version:       "1.1.0",
		ArchivePath:   "ver-skill/1.1.0.tar.gz",
		ArchiveSize:   2222,
		ArchiveSHA256: "feedface",
	}
	if err := skills.CreateVersion(v2); err != nil {
		t.Fatalf("CreateVersion v2: %v", err)
	}
	got, _ = skills.GetSkill(sk.ID)
	if got.LatestVersion != "1.1.0" {
		t.Errorf("LatestVersion after v2 = %q, want 1.1.0", got.LatestVersion)
	}

	dup := &store.SkillVersion{
		SkillID:       sk.ID,
		Version:       "1.0.0",
		ArchivePath:   "ver-skill/1.0.0.tar.gz",
		ArchiveSize:   1234,
		ArchiveSHA256: "deadbeef",
	}
	err = skills.CreateVersion(dup)
	if !errors.Is(err, store.ErrSkillVersionConflict) {
		t.Fatalf("expected ErrSkillVersionConflict on duplicate, got %v", err)
	}
}

func TestSkillCreateVersion_RequiresFields(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)
	sk := &store.Skill{Slug: "field-skill", DisplayName: "x"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	cases := []*store.SkillVersion{
		{SkillID: sk.ID, ArchivePath: "p", ArchiveSize: 1, ArchiveSHA256: "h"}, // no version
		{SkillID: sk.ID, Version: "1.0.0", ArchiveSize: 1, ArchiveSHA256: "h"}, // no archive path
		{SkillID: sk.ID, Version: "1.0.0", ArchivePath: "p", ArchiveSHA256: "h"}, // size = 0
		{SkillID: sk.ID, Version: "1.0.0", ArchivePath: "p", ArchiveSize: 1},     // no sha
	}
	for i, c := range cases {
		if err := skills.CreateVersion(c); err == nil {
			t.Errorf("case %d: expected validation failure", i)
		}
	}
}

func TestSkillListAndYank(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	for _, slug := range []string{"alpha", "bravo", "charlie"} {
		if err := skills.CreateSkill(&store.Skill{Slug: slug, DisplayName: slug}); err != nil {
			t.Fatalf("CreateSkill %s: %v", slug, err)
		}
	}
	all, err := skills.ListSkills()
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListSkills len = %d, want 3", len(all))
	}

	if err := skills.YankSkill(all[0].ID); err != nil {
		t.Fatalf("YankSkill: %v", err)
	}
	got, _ := skills.GetSkill(all[0].ID)
	if got.YankedAt == nil {
		t.Errorf("YankedAt should be set after YankSkill")
	}
	if err := skills.UnyankSkill(all[0].ID); err != nil {
		t.Fatalf("UnyankSkill: %v", err)
	}
	got, _ = skills.GetSkill(all[0].ID)
	if got.YankedAt != nil {
		t.Errorf("YankedAt should be nil after UnyankSkill")
	}
}

func TestSkillVersionYank(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{Slug: "yank-skill", DisplayName: "Y"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	v := &store.SkillVersion{
		SkillID: sk.ID, Version: "1.0.0",
		ArchivePath: "yank-skill/1.0.0.tar.gz", ArchiveSize: 10, ArchiveSHA256: "h",
	}
	if err := skills.CreateVersion(v); err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if err := skills.YankVersion(sk.ID, "1.0.0"); err != nil {
		t.Fatalf("YankVersion: %v", err)
	}
	got, _ := skills.GetVersion(sk.ID, "1.0.0")
	if got.YankedAt == nil {
		t.Error("YankedAt should be set")
	}
	if err := skills.UnyankVersion(sk.ID, "1.0.0"); err != nil {
		t.Fatalf("UnyankVersion: %v", err)
	}
	got, _ = skills.GetVersion(sk.ID, "1.0.0")
	if got.YankedAt != nil {
		t.Error("YankedAt should be cleared")
	}
}

func TestSkillAssignAndAssignedForUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)
	users := store.NewUserStore(db)

	user, err := users.Create("alice", "secret-pw", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	pubSk := &store.Skill{Slug: "shared-skill", DisplayName: "Shared", Visibility: "public"}
	if err := skills.CreateSkill(pubSk); err != nil {
		t.Fatalf("CreateSkill public: %v", err)
	}
	restSk := &store.Skill{Slug: "team-skill", DisplayName: "Team", Visibility: "restricted"}
	if err := skills.CreateSkill(restSk); err != nil {
		t.Fatalf("CreateSkill restricted: %v", err)
	}
	hiddenSk := &store.Skill{Slug: "secret-skill", DisplayName: "Secret", Visibility: "restricted"}
	if err := skills.CreateSkill(hiddenSk); err != nil {
		t.Fatalf("CreateSkill secret: %v", err)
	}

	// Without any assignment, alice sees only the public skill.
	assigned, err := skills.AssignedForUser(user.ID)
	if err != nil {
		t.Fatalf("AssignedForUser pre: %v", err)
	}
	if len(assigned) != 1 || assigned[0].Skill.Slug != "shared-skill" {
		t.Fatalf("expected only shared-skill, got %+v", assigned)
	}

	// Assign restricted; now alice sees two.
	if err := skills.AssignSkill(&store.SkillAssignment{
		SkillID: restSk.ID, UserID: user.ID,
	}); err != nil {
		t.Fatalf("AssignSkill: %v", err)
	}
	assigned, _ = skills.AssignedForUser(user.ID)
	if len(assigned) != 2 {
		t.Fatalf("expected 2 assigned, got %d", len(assigned))
	}

	// Pin a specific version on the restricted skill.
	pin := "1.2.3"
	if err := skills.AssignSkill(&store.SkillAssignment{
		SkillID: restSk.ID, UserID: user.ID, Version: &pin,
	}); err != nil {
		t.Fatalf("AssignSkill pin: %v", err)
	}
	assigned, _ = skills.AssignedForUser(user.ID)
	for _, a := range assigned {
		if a.Skill.ID == restSk.ID {
			if a.PinnedVersion == nil || *a.PinnedVersion != pin {
				t.Errorf("expected pinned %q, got %v", pin, a.PinnedVersion)
			}
		}
	}

	// Yanking a public skill removes it from listings even without revoking access.
	if err := skills.YankSkill(pubSk.ID); err != nil {
		t.Fatalf("YankSkill: %v", err)
	}
	assigned, _ = skills.AssignedForUser(user.ID)
	if len(assigned) != 1 {
		t.Fatalf("expected 1 after yank, got %d", len(assigned))
	}

	// Unassign drops the user back to public-only (empty after yank above).
	if err := skills.UnassignSkill(restSk.ID, user.ID); err != nil {
		t.Fatalf("UnassignSkill: %v", err)
	}
	assigned, _ = skills.AssignedForUser(user.ID)
	if len(assigned) != 0 {
		t.Fatalf("expected 0 after unassign+yank, got %d", len(assigned))
	}
}

func TestSkillUpdateMeta(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{Slug: "meta", DisplayName: "Old"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	if err := skills.UpdateSkillMeta(sk.ID, "New", "New desc", "public"); err != nil {
		t.Fatalf("UpdateSkillMeta: %v", err)
	}
	got, _ := skills.GetSkill(sk.ID)
	if got.DisplayName != "New" || got.Description != "New desc" || got.Visibility != "public" {
		t.Errorf("UpdateSkillMeta did not apply: %+v", got)
	}
	// Empty visibility must NOT clear the field.
	if err := skills.UpdateSkillMeta(sk.ID, "Newer", "still", ""); err != nil {
		t.Fatalf("UpdateSkillMeta empty vis: %v", err)
	}
	got, _ = skills.GetSkill(sk.ID)
	if got.Visibility != "public" {
		t.Errorf("Visibility cleared on empty input: %+v", got)
	}
}

func TestSkillListVersionsOrder(t *testing.T) {
	db := testutil.OpenTestDB(t)
	skills := store.NewSkillStore(db)

	sk := &store.Skill{Slug: "ord-skill", DisplayName: "x"}
	if err := skills.CreateSkill(sk); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		if err := skills.CreateVersion(&store.SkillVersion{
			SkillID: sk.ID, Version: v,
			ArchivePath: sk.Slug + "/" + v + ".tar.gz",
			ArchiveSize: 1, ArchiveSHA256: "h",
		}); err != nil {
			t.Fatalf("CreateVersion %s: %v", v, err)
		}
	}
	versions, err := skills.ListVersions(sk.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(versions))
	}
	// Newest upload is returned first.
	if versions[0].Version != "2.0.0" {
		t.Errorf("first version = %q, want 2.0.0", versions[0].Version)
	}
}
