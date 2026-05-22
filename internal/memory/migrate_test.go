package memory

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/felixgeelhaar/mnemos"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

func setupLegacyDB(t *testing.T) (*db.DB, *Manager, func()) {
	t.Helper()
	tmp, err := os.CreateTemp("", "nomi-legacy-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	database, err := db.New(db.Config{Path: tmp.Name()})
	if err != nil {
		os.Remove(tmp.Name())
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		database.Close()
		os.Remove(tmp.Name())
		t.Fatal(err)
	}
	mgr := NewManager(db.NewMemoryRepository(database))
	cleanup := func() {
		database.Close()
		os.Remove(tmp.Name())
	}
	return database, mgr, cleanup
}

func TestMigrateLegacyMemory_CopiesAndMarksComplete(t *testing.T) {
	legacyDB, mgr, cleanup := setupLegacyDB(t)
	defer cleanup()

	// Seed legacy rows across the three scopes the runtime ever wrote.
	for _, e := range []*domain.MemoryEntry{
		{Scope: "workspace", Content: "ws-1", CreatedAt: time.Now().UTC()},
		{Scope: "workspace", Content: "ws-2", CreatedAt: time.Now().UTC()},
		{Scope: "profile", Content: "pf-1", CreatedAt: time.Now().UTC()},
		{Scope: "preferences", Content: "pref-1", CreatedAt: time.Now().UTC()},
	} {
		if err := mgr.Save(e); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}
	}

	dst := NewTestClient(t)
	if err := MigrateLegacyMemory(context.Background(), legacyDB, dst, mgr); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Verify every scope round-tripped.
	ws, _ := dst.Retrieve(context.Background(), mnemos.LocalWorkspace(), mnemos.Query{Limit: 10})
	if len(ws) != 2 {
		t.Errorf("workspace count = %d, want 2", len(ws))
	}
	pf, _ := dst.Retrieve(context.Background(), mnemos.LocalProfile(), mnemos.Query{Limit: 10})
	if len(pf) != 1 {
		t.Errorf("profile count = %d, want 1", len(pf))
	}
	pref, _ := dst.Retrieve(context.Background(), mnemos.LocalPreferences(), mnemos.Query{Limit: 10})
	if len(pref) != 1 {
		t.Errorf("preferences count = %d, want 1", len(pref))
	}

	// Marker recorded.
	settings := db.NewAppSettingsRepository(legacyDB)
	mark, err := settings.Get(migrationCompletedKey)
	if err != nil || mark == "" {
		t.Errorf("completion marker missing: %v / %q", err, mark)
	}
}

func TestMigrateLegacyMemory_Idempotent(t *testing.T) {
	legacyDB, mgr, cleanup := setupLegacyDB(t)
	defer cleanup()
	if err := mgr.Save(&domain.MemoryEntry{Scope: "workspace", Content: "one", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	dst := NewTestClient(t)
	if err := MigrateLegacyMemory(context.Background(), legacyDB, dst, mgr); err != nil {
		t.Fatal(err)
	}
	// Second run should be a no-op (no rows duplicated, no error).
	if err := MigrateLegacyMemory(context.Background(), legacyDB, dst, mgr); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got, _ := dst.Retrieve(context.Background(), mnemos.LocalWorkspace(), mnemos.Query{Limit: 10})
	if len(got) != 1 {
		t.Errorf("want exactly 1 row after re-run, got %d", len(got))
	}
}

func TestMigrateLegacyMemory_EmptyLegacyIsNoop(t *testing.T) {
	legacyDB, mgr, cleanup := setupLegacyDB(t)
	defer cleanup()

	dst := NewTestClient(t)
	if err := MigrateLegacyMemory(context.Background(), legacyDB, dst, mgr); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, _ := dst.Retrieve(context.Background(), mnemos.LocalWorkspace(), mnemos.Query{Limit: 10})
	if len(got) != 0 {
		t.Errorf("want empty, got %d", len(got))
	}
	// Marker should still be set so subsequent boots skip.
	settings := db.NewAppSettingsRepository(legacyDB)
	if mark, _ := settings.Get(migrationCompletedKey); mark == "" {
		t.Error("completion marker should be set even on empty legacy")
	}
}
