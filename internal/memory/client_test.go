package memory

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/mnemos"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// seedAssistant inserts a minimal AssistantDefinition so memory rows can
// take the FK.
func seedAssistant(t *testing.T, database *db.DB, id string) {
	t.Helper()
	repo := db.NewAssistantRepository(database)
	a := &domain.AssistantDefinition{
		ID:           id,
		Name:         "test-" + id,
		Role:         "test",
		SystemPrompt: "test",
		MemoryPolicy: domain.MemoryPolicy{Enabled: true, Scope: "workspace"},
		CreatedAt:    time.Now().UTC(),
	}
	if err := repo.Create(a); err != nil {
		t.Fatalf("seedAssistant: %v", err)
	}
}

// seedRun inserts a minimal Run so memory rows can take the FK.
func seedRun(t *testing.T, database *db.DB, id, assistantID string) {
	t.Helper()
	repo := db.NewRunRepository(database)
	r := &domain.Run{
		ID:          id,
		Goal:        "test",
		AssistantID: assistantID,
		Status:      domain.RunCreated,
		PlanVersion: 1,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := repo.Create(r); err != nil {
		t.Fatalf("seedRun: %v", err)
	}
}

func newTestClient(t *testing.T) (*EmbeddedClient, *db.DB, *db.MemoryRepository, func()) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "nomi-mnemos-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpFile.Close()

	database, err := db.New(db.Config{Path: tmpFile.Name()})
	if err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("open db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		database.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("migrate: %v", err)
	}
	repo := db.NewMemoryRepository(database)
	c := NewEmbeddedClient(repo)
	cleanup := func() {
		database.Close()
		os.Remove(tmpFile.Name())
	}
	return c, database, repo, cleanup
}

func TestEmbeddedClient_StoreAssignsAndHashes(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	entry := &mnemos.Entry{Content: "hello world"}
	if err := c.Store(context.Background(), mnemos.LocalWorkspace(), entry); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if entry.ID == "" {
		t.Error("Store should assign ID when empty")
	}
	if entry.CreatedAt.IsZero() {
		t.Error("Store should stamp CreatedAt when zero")
	}
	if entry.ContentHash == "" {
		t.Error("Store should compute ContentHash")
	}
	// SHA-256 of "hello world"
	const want = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if entry.ContentHash != want {
		t.Errorf("ContentHash = %q, want %q", entry.ContentHash, want)
	}
}

func TestEmbeddedClient_RoundTripWorkspace(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	for _, body := range []string{"alpha", "beta", "gamma"} {
		if err := c.Store(ctx, scope, &mnemos.Entry{Content: body}); err != nil {
			t.Fatalf("Store %q: %v", body, err)
		}
	}

	got, err := c.Retrieve(ctx, scope, mnemos.Query{Limit: 10})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("want 3 entries, got %d", len(got))
	}
}

func TestEmbeddedClient_RetrieveDoesNotCrossScopes(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()

	if err := c.Store(ctx, mnemos.LocalWorkspace(), &mnemos.Entry{Content: "ws"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(ctx, mnemos.LocalProfile(), &mnemos.Entry{Content: "pf"}); err != nil {
		t.Fatal(err)
	}

	ws, err := c.Retrieve(ctx, mnemos.LocalWorkspace(), mnemos.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Content != "ws" {
		t.Errorf("workspace retrieve: %+v", ws)
	}

	pf, err := c.Retrieve(ctx, mnemos.LocalProfile(), mnemos.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(pf) != 1 || pf[0].Content != "pf" {
		t.Errorf("profile retrieve: %+v", pf)
	}
}

func TestEmbeddedClient_RetrieveQueryFilters(t *testing.T) {
	c, database, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	aID := "assistant-A"
	bID := "assistant-B"
	seedAssistant(t, database, aID)
	seedAssistant(t, database, bID)
	if err := c.Store(ctx, scope, &mnemos.Entry{Content: "from A", AssistantID: &aID}); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(ctx, scope, &mnemos.Entry{Content: "from B", AssistantID: &bID}); err != nil {
		t.Fatal(err)
	}

	got, err := c.Retrieve(ctx, scope, mnemos.Query{AssistantID: &aID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "from A" {
		t.Errorf("AssistantID filter: %+v", got)
	}
}

func TestEmbeddedClient_RetrieveSinceFilter(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	old := &mnemos.Entry{Content: "old", CreatedAt: time.Now().UTC().Add(-2 * time.Hour)}
	if err := c.Store(ctx, scope, old); err != nil {
		t.Fatal(err)
	}
	fresh := &mnemos.Entry{Content: "fresh"}
	if err := c.Store(ctx, scope, fresh); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC().Add(-1 * time.Hour)
	got, err := c.Retrieve(ctx, scope, mnemos.Query{Since: &cutoff, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "fresh" {
		t.Errorf("Since filter: %+v", got)
	}
}

func TestEmbeddedClient_SearchSubstring(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	for _, body := range []string{"AuthN failed", "auth required", "unrelated"} {
		if err := c.Store(ctx, scope, &mnemos.Entry{Content: body}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := c.Search(ctx, scope, "auth", mnemos.SearchOpts{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Search 'auth' want 2, got %d (%+v)", len(got), got)
	}
}

func TestEmbeddedClient_ForgetDeletes(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	entry := &mnemos.Entry{Content: "x"}
	if err := c.Store(ctx, scope, entry); err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(ctx, scope, entry.ID); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	got, err := c.Retrieve(ctx, scope, mnemos.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want empty after Forget, got %d", len(got))
	}
}

func TestEmbeddedClient_ForgetUnknownReturnsNotFound(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	err := c.Forget(context.Background(), mnemos.LocalWorkspace(), "no-such-id")
	if !errors.Is(err, mnemos.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestEmbeddedClient_ForgetRejectsScopeMismatch(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()

	entry := &mnemos.Entry{Content: "x"}
	if err := c.Store(ctx, mnemos.LocalWorkspace(), entry); err != nil {
		t.Fatal(err)
	}
	// Try forgetting from profile scope — should look like not-found.
	err := c.Forget(ctx, mnemos.LocalProfile(), entry.ID)
	if !errors.Is(err, mnemos.ErrNotFound) {
		t.Errorf("cross-scope Forget: want ErrNotFound, got %v", err)
	}
}

func TestEmbeddedClient_TombstoneAssistantAnonymizes(t *testing.T) {
	c, database, repo, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	aID := "assistant-to-delete"
	seedAssistant(t, database, aID)
	if err := c.Store(ctx, scope, &mnemos.Entry{Content: "keep me", AssistantID: &aID}); err != nil {
		t.Fatal(err)
	}

	if err := c.Tombstone(ctx, mnemos.EntityRef{Kind: mnemos.EntityAssistant, ID: aID}); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}

	rows, err := repo.ListByScope("workspace", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("row should survive tombstone, got %d rows", len(rows))
	}
	if rows[0].AssistantID != nil {
		t.Errorf("AssistantID should be nil after tombstone, got %v", *rows[0].AssistantID)
	}
}

func TestEmbeddedClient_TombstoneRunAnonymizes(t *testing.T) {
	c, database, repo, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()
	scope := mnemos.LocalWorkspace()

	aID := "assistant-for-run"
	rID := "run-to-delete"
	seedAssistant(t, database, aID)
	seedRun(t, database, rID, aID)
	if err := c.Store(ctx, scope, &mnemos.Entry{Content: "keep me", RunID: &rID}); err != nil {
		t.Fatal(err)
	}

	if err := c.Tombstone(ctx, mnemos.EntityRef{Kind: mnemos.EntityRun, ID: rID}); err != nil {
		t.Fatal(err)
	}

	rows, err := repo.ListByScope("workspace", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != nil {
		t.Errorf("row should survive with nil RunID; got %+v", rows[0])
	}
}

func TestEmbeddedClient_TombstoneIdempotent(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()
	ctx := context.Background()

	ref := mnemos.EntityRef{Kind: mnemos.EntityAssistant, ID: "never-existed"}
	for i := range 3 {
		if err := c.Tombstone(ctx, ref); err != nil {
			t.Errorf("Tombstone iter %d: %v", i, err)
		}
	}
}

func TestEmbeddedClient_StoreRejectsInvalidScope(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	err := c.Store(context.Background(), mnemos.Scope{}, &mnemos.Entry{Content: "x"})
	if !errors.Is(err, mnemos.ErrInvalidScope) {
		t.Errorf("want ErrInvalidScope, got %v", err)
	}
}

func TestEmbeddedClient_RetrieveCancelledContext(t *testing.T) {
	c, _, _, cleanup := newTestClient(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Retrieve(ctx, mnemos.LocalWorkspace(), mnemos.Query{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}
