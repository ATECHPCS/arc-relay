package store_test

import (
	"encoding/json"
	"testing"

	"github.com/JeremiahChurch/mcp-wrangler/internal/store"
	"github.com/JeremiahChurch/mcp-wrangler/internal/testutil"
)

func TestServerCreate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)

	srv := &store.Server{
		Name:        "test-server",
		DisplayName: "Test Server",
		ServerType:  store.ServerTypeStdio,
		Config:      json.RawMessage(`{"image":"test:latest"}`),
	}

	if err := servers.Create(srv); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if srv.ID == "" {
		t.Error("ID should be generated")
	}
	if srv.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", srv.Status, store.StatusStopped)
	}
	if srv.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if srv.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}
}

func TestServerGetAndGetByName(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)

	srv := &store.Server{
		Name:        "findme",
		DisplayName: "Find Me",
		ServerType:  store.ServerTypeRemote,
		Config:      json.RawMessage(`{"url":"https://example.com"}`),
	}
	servers.Create(srv)

	t.Run("Get found", func(t *testing.T) {
		found, err := servers.Get(srv.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if found == nil {
			t.Fatal("Get() returned nil")
		}
		if found.Name != "findme" {
			t.Errorf("Name = %q, want %q", found.Name, "findme")
		}
		if found.ServerType != store.ServerTypeRemote {
			t.Errorf("ServerType = %q, want %q", found.ServerType, store.ServerTypeRemote)
		}
	})

	t.Run("Get not found", func(t *testing.T) {
		found, err := servers.Get("nonexistent")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if found != nil {
			t.Error("Get() should return nil for nonexistent ID")
		}
	})

	t.Run("GetByName found", func(t *testing.T) {
		found, err := servers.GetByName("findme")
		if err != nil {
			t.Fatalf("GetByName() error = %v", err)
		}
		if found == nil {
			t.Fatal("GetByName() returned nil")
		}
		if found.ID != srv.ID {
			t.Errorf("ID = %q, want %q", found.ID, srv.ID)
		}
	})

	t.Run("GetByName not found", func(t *testing.T) {
		found, err := servers.GetByName("nonexistent")
		if err != nil {
			t.Fatalf("GetByName() error = %v", err)
		}
		if found != nil {
			t.Error("GetByName() should return nil for nonexistent name")
		}
	})
}

func TestServerList(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)

	t.Run("empty", func(t *testing.T) {
		list, err := servers.List()
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(list) != 0 {
			t.Errorf("List() returned %d servers, want 0", len(list))
		}
	})

	servers.Create(&store.Server{Name: "s1", DisplayName: "S1", ServerType: store.ServerTypeStdio, Config: json.RawMessage(`{}`)})
	servers.Create(&store.Server{Name: "s2", DisplayName: "S2", ServerType: store.ServerTypeHTTP, Config: json.RawMessage(`{}`)})

	t.Run("populated", func(t *testing.T) {
		list, err := servers.List()
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(list) != 2 {
			t.Errorf("List() returned %d servers, want 2", len(list))
		}
	})
}

func TestServerUpdate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)

	srv := &store.Server{Name: "updatable", DisplayName: "Updatable", ServerType: store.ServerTypeStdio, Config: json.RawMessage(`{}`)}
	servers.Create(srv)
	originalUpdatedAt := srv.UpdatedAt

	srv.DisplayName = "Updated Name"
	srv.Status = store.StatusRunning
	if err := servers.Update(srv); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	found, _ := servers.Get(srv.ID)
	if found.DisplayName != "Updated Name" {
		t.Errorf("DisplayName = %q, want %q", found.DisplayName, "Updated Name")
	}
	if found.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", found.Status, store.StatusRunning)
	}
	if !found.UpdatedAt.After(originalUpdatedAt) {
		t.Error("UpdatedAt should be advanced after Update()")
	}
}

func TestServerUpdateStatus(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)

	srv := &store.Server{Name: "statustest", DisplayName: "Status Test", ServerType: store.ServerTypeStdio, Config: json.RawMessage(`{}`)}
	servers.Create(srv)

	if err := servers.UpdateStatus(srv.ID, store.StatusError, "connection refused"); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	found, _ := servers.Get(srv.ID)
	if found.Status != store.StatusError {
		t.Errorf("Status = %q, want %q", found.Status, store.StatusError)
	}
	if found.ErrorMsg != "connection refused" {
		t.Errorf("ErrorMsg = %q, want %q", found.ErrorMsg, "connection refused")
	}
}

func TestServerUpdateConfig(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)

	srv := &store.Server{Name: "cfgtest", DisplayName: "Config Test", ServerType: store.ServerTypeHTTP, Config: json.RawMessage(`{"url":"old"}`)}
	servers.Create(srv)

	newConfig := json.RawMessage(`{"url":"new","port":9090}`)
	if err := servers.UpdateConfig(srv.ID, newConfig); err != nil {
		t.Fatalf("UpdateConfig() error = %v", err)
	}

	found, _ := servers.Get(srv.ID)
	var cfg map[string]interface{}
	json.Unmarshal(found.Config, &cfg)
	if cfg["url"] != "new" {
		t.Errorf("config url = %v, want %q", cfg["url"], "new")
	}
}

func TestServerDeleteCascadesAccessTiers(t *testing.T) {
	db := testutil.OpenTestDB(t)
	servers := store.NewServerStore(db)
	access := store.NewAccessStore(db)

	srv := &store.Server{Name: "cascade", DisplayName: "Cascade", ServerType: store.ServerTypeStdio, Config: json.RawMessage(`{}`)}
	servers.Create(srv)

	// Add an access tier for this server
	access.SetTier(srv.ID, "tool", "test_tool", "read")

	// Delete server
	if err := servers.Delete(srv.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Verify server is gone
	found, _ := servers.Get(srv.ID)
	if found != nil {
		t.Error("server should be deleted")
	}

	// Verify access tiers were cascade-deleted
	tiers, _ := access.GetAllTiers(srv.ID)
	if len(tiers) != 0 {
		t.Errorf("access tiers should be cascade-deleted, got %d remaining", len(tiers))
	}
}
