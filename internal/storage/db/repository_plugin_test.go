package db

import (
	"os"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func newTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "nomi-plugin-*.db")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	f.Close()
	database, err := New(Config{Path: f.Name()})
	if err != nil {
		os.Remove(f.Name())
		t.Fatalf("open db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		database.Close()
		os.Remove(f.Name())
		t.Fatalf("migrate: %v", err)
	}
	return database, func() {
		database.Close()
		os.Remove(f.Name())
	}
}

// seedAssistant inserts a minimal assistant row so the foreign-key
// constraint on assistant_connection_bindings is satisfied. The full
// AssistantDefinition isn't needed for binding tests.
func seedAssistant(t *testing.T, database *DB, id string) {
	t.Helper()
	_, err := database.Exec(
		`INSERT INTO assistants (id, name, role, system_prompt, capabilities, channels, contexts, memory_policy, permission_policy)
		 VALUES (?, 'Test', 'assistant', '', '[]', '[]', '[]', '{}', '{}')`,
		id,
	)
	if err != nil {
		t.Fatalf("seed assistant %s: %v", id, err)
	}
}

func TestConnectionRepository_CreateGetList(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewConnectionRepository(database)
	conn := &domain.Connection{
		ID:       "conn-1",
		PluginID: "com.nomi.telegram",
		Name:     "Work Bot",
		Config:   map[string]any{"api_base": "https://api.telegram.org"},
		CredentialRefs: map[string]string{
			"bot_token": "secret://telegram/conn-1/bot_token",
		},
		Enabled: true,
	}
	if err := repo.Create(conn); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID("conn-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PluginID != "com.nomi.telegram" || got.Name != "Work Bot" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.CredentialRefs["bot_token"] != "secret://telegram/conn-1/bot_token" {
		t.Fatalf("credential ref lost: %+v", got.CredentialRefs)
	}
	if got.Config["api_base"] != "https://api.telegram.org" {
		t.Fatalf("config lost: %+v", got.Config)
	}

	list, err := repo.ListByPlugin("com.nomi.telegram")
	if err != nil {
		t.Fatalf("ListByPlugin: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListByPlugin should return 1, got %d", len(list))
	}
}

func TestConnectionRepository_ListEnabled_FiltersDisabled(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewConnectionRepository(database)
	_ = repo.Create(&domain.Connection{ID: "a", PluginID: "com.nomi.x", Name: "A", Enabled: true})
	_ = repo.Create(&domain.Connection{ID: "b", PluginID: "com.nomi.x", Name: "B", Enabled: false})

	got, err := repo.ListEnabled()
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("ListEnabled should surface only enabled rows, got %+v", got)
	}
}

func TestConnectionRepository_UpdateAndDelete(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	repo := NewConnectionRepository(database)
	conn := &domain.Connection{ID: "c", PluginID: "com.nomi.x", Name: "orig", Enabled: true}
	if err := repo.Create(conn); err != nil {
		t.Fatalf("Create: %v", err)
	}
	conn.Name = "renamed"
	conn.Enabled = false
	if err := repo.Update(conn); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := repo.GetByID("c")
	if got.Name != "renamed" || got.Enabled {
		t.Fatalf("Update didn't persist: %+v", got)
	}

	if err := repo.Delete("c"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID("c"); err == nil {
		t.Fatal("Delete should have removed the row")
	}
}

func TestAssistantBindingRepository_UpsertAndResolvePrimary(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	bindRepo := NewAssistantBindingRepository(database)
	seedAssistant(t, database, "asst-1")

	// Two Gmail connections, assistant bound to both with one marked primary.
	_ = connRepo.Create(&domain.Connection{ID: "gm-work", PluginID: "com.nomi.gmail", Name: "Work", Enabled: true})
	_ = connRepo.Create(&domain.Connection{ID: "gm-personal", PluginID: "com.nomi.gmail", Name: "Personal", Enabled: true})

	if err := bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID: "asst-1", ConnectionID: "gm-work", Role: domain.BindingRoleTool, Enabled: true, IsPrimary: true,
	}); err != nil {
		t.Fatalf("upsert primary: %v", err)
	}
	if err := bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID: "asst-1", ConnectionID: "gm-personal", Role: domain.BindingRoleTool, Enabled: true, IsPrimary: false,
	}); err != nil {
		t.Fatalf("upsert secondary: %v", err)
	}

	got, err := bindRepo.ResolvePrimary("asst-1", "com.nomi.gmail", domain.BindingRoleTool)
	if err != nil {
		t.Fatalf("ResolvePrimary: %v", err)
	}
	if got == nil || got.ConnectionID != "gm-work" {
		t.Fatalf("ResolvePrimary should return the primary binding, got %+v", got)
	}
}

func TestAssistantBindingRepository_PrimaryUniquenessPerGroup(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	bindRepo := NewAssistantBindingRepository(database)
	seedAssistant(t, database, "asst-1")
	_ = connRepo.Create(&domain.Connection{ID: "a", PluginID: "com.nomi.gmail", Name: "A", Enabled: true})
	_ = connRepo.Create(&domain.Connection{ID: "b", PluginID: "com.nomi.gmail", Name: "B", Enabled: true})

	// Mark A primary, then mark B primary — A must lose its primary flag.
	_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID: "asst-1", ConnectionID: "a", Role: domain.BindingRoleTool, Enabled: true, IsPrimary: true,
	})
	_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID: "asst-1", ConnectionID: "b", Role: domain.BindingRoleTool, Enabled: true, IsPrimary: true,
	})

	all, err := bindRepo.ListByAssistant("asst-1")
	if err != nil {
		t.Fatalf("ListByAssistant: %v", err)
	}
	var primaryCount int
	for _, b := range all {
		if b.IsPrimary {
			primaryCount++
		}
	}
	if primaryCount != 1 {
		t.Fatalf("expected exactly 1 primary per (assistant, plugin, role); got %d", primaryCount)
	}
}

func TestAssistantBindingRepository_HasBinding(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	bindRepo := NewAssistantBindingRepository(database)
	seedAssistant(t, database, "asst-1")
	_ = connRepo.Create(&domain.Connection{ID: "c", PluginID: "com.nomi.gmail", Name: "c", Enabled: true})

	ok, _ := bindRepo.HasBinding("asst-1", "c", domain.BindingRoleTool)
	if ok {
		t.Fatal("should not have binding yet")
	}
	_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID: "asst-1", ConnectionID: "c", Role: domain.BindingRoleTool, Enabled: true,
	})
	ok, _ = bindRepo.HasBinding("asst-1", "c", domain.BindingRoleTool)
	if !ok {
		t.Fatal("should have binding after upsert")
	}
	// Different role — no binding.
	ok, _ = bindRepo.HasBinding("asst-1", "c", domain.BindingRoleChannel)
	if ok {
		t.Fatal("HasBinding should be role-scoped")
	}
}

func TestAssistantBindingRepository_CascadesOnConnectionDelete(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := NewConnectionRepository(database)
	bindRepo := NewAssistantBindingRepository(database)
	seedAssistant(t, database, "asst-1")
	_ = connRepo.Create(&domain.Connection{ID: "c", PluginID: "com.nomi.gmail", Name: "c", Enabled: true})
	_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID: "asst-1", ConnectionID: "c", Role: domain.BindingRoleTool, Enabled: true,
	})
	// Connection delete must cascade through the junction.
	if err := connRepo.Delete("c"); err != nil {
		t.Fatalf("Delete connection: %v", err)
	}
	bindings, _ := bindRepo.ListByAssistant("asst-1")
	if len(bindings) != 0 {
		t.Fatalf("bindings should have cascaded on connection delete, got %+v", bindings)
	}
}

func TestBindingRole_Validation(t *testing.T) {
	if !domain.BindingRoleChannel.IsValid() {
		t.Fatal("channel should be valid")
	}
	if domain.BindingRole("nope").IsValid() {
		t.Fatal("unknown role should be invalid")
	}
}
