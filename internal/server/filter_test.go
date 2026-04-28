package server

import (
	"encoding/json"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/mcp"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

// makeToolsResp builds a JSON-RPC response carrying the named tools in tools/list shape.
func makeToolsResp(t *testing.T, names ...string) *mcp.Response {
	t.Helper()
	tools := make([]mcp.Tool, 0, len(names))
	for _, n := range names {
		tools = append(tools, mcp.Tool{Name: n})
	}
	raw, err := json.Marshal(mcp.ToolsListResult{Tools: tools})
	if err != nil {
		t.Fatalf("marshal tools result: %v", err)
	}
	return &mcp.Response{Result: raw}
}

func decodeToolNames(t *testing.T, resp *mcp.Response) []string {
	t.Helper()
	var got mcp.ToolsListResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal filtered result: %v", err)
	}
	out := make([]string, 0, len(got.Tools))
	for _, tool := range got.Tools {
		out = append(out, tool.Name)
	}
	return out
}

// TestFilterListResponse_AdminBypassesProfileFilter is the regression test for the
// /mcp/{server} tools/list empty-array bug observed in production 2026-04-28. An admin
// user whose effective profile happens to grant zero permissions for a given server
// must still see the upstream tools list — admins bypass access checks on tools/call
// (see checkEndpointAccess), so the discovery path must match.
func TestFilterListResponse_AdminBypassesProfileFilter(t *testing.T) {
	db := testutil.OpenTestDB(t)
	profiles := store.NewProfileStore(db)

	prof, err := profiles.Create("locked-down", "no perms for srv-outline")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}

	s := &Server{profileStore: profiles}
	admin := &store.User{
		ID:        "admin-1",
		Username:  "admin",
		Role:      "admin",
		ProfileID: &prof.ID,
	}

	resp := makeToolsResp(t, "create_document", "update_document", "fetch")
	out := s.filterListResponse(resp, "tools/list", admin, "srv-outline")

	got := decodeToolNames(t, out)
	if len(got) != 3 {
		t.Errorf("admin must see all upstream tools regardless of profile perms; got %d (%v)", len(got), got)
	}
}

// TestFilterListResponse_NonAdminFilteredToPermitted guards the non-admin path:
// a regular user only sees tools their profile actually grants.
func TestFilterListResponse_NonAdminFilteredToPermitted(t *testing.T) {
	db := testutil.OpenTestDB(t)
	profiles := store.NewProfileStore(db)

	if _, err := db.Exec(`INSERT INTO servers (id, name, display_name, server_type, config, status, created_at, updated_at)
		VALUES ('srv-outline', 'outline', 'Outline', 'remote', '{}', 'stopped', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed server row: %v", err)
	}
	prof, err := profiles.Create("partial", "two of three tools")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := profiles.BulkSetPermissions(prof.ID, "srv-outline", []store.ProfilePermission{
		{ProfileID: prof.ID, ServerID: "srv-outline", EndpointType: "tool", EndpointName: "create_document"},
		{ProfileID: prof.ID, ServerID: "srv-outline", EndpointType: "tool", EndpointName: "fetch"},
	}); err != nil {
		t.Fatalf("set permissions: %v", err)
	}

	s := &Server{profileStore: profiles}
	user := &store.User{
		ID:        "user-1",
		Username:  "alice",
		Role:      "user",
		ProfileID: &prof.ID,
	}

	resp := makeToolsResp(t, "create_document", "update_document", "fetch")
	out := s.filterListResponse(resp, "tools/list", user, "srv-outline")

	got := decodeToolNames(t, out)
	if len(got) != 2 {
		t.Fatalf("non-admin should see 2 permitted tools; got %d (%v)", len(got), got)
	}
	wantSet := map[string]bool{"create_document": true, "fetch": true}
	for _, name := range got {
		if !wantSet[name] {
			t.Errorf("unexpected tool in filtered output: %q", name)
		}
	}
}

// TestFilterListResponse_NonAdminEmptyProfile guards the existing behavior: a
// non-admin whose profile has no permissions for the server sees an empty tools
// list (matching their inability to invoke any tool on it).
func TestFilterListResponse_NonAdminEmptyProfile(t *testing.T) {
	db := testutil.OpenTestDB(t)
	profiles := store.NewProfileStore(db)

	prof, err := profiles.Create("empty", "no perms")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}

	s := &Server{profileStore: profiles}
	user := &store.User{
		ID:        "user-2",
		Username:  "bob",
		Role:      "user",
		ProfileID: &prof.ID,
	}

	resp := makeToolsResp(t, "create_document", "update_document")
	out := s.filterListResponse(resp, "tools/list", user, "srv-outline")

	got := decodeToolNames(t, out)
	if len(got) != 0 {
		t.Errorf("non-admin with empty profile should see zero tools; got %d (%v)", len(got), got)
	}
}
