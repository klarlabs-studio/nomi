package db

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

func setupMemoryFTSTest(t *testing.T) *MemoryRepository {
	t.Helper()
	conn, err := New(Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewMemoryRepository(conn)
}

func seedMem(t *testing.T, r *MemoryRepository, content string) string {
	t.Helper()
	id := uuid.New().String()
	if err := r.Create(&domain.MemoryEntry{
		ID:        id,
		Scope:     "workspace",
		Content:   content,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

func TestSearchUsesFTSAndRanks(t *testing.T) {
	r := setupMemoryFTSTest(t)
	seedMem(t, r, "Run the integration tests before merging")
	seedMem(t, r, "Coffee preferences: oat milk")
	seedMem(t, r, "Always run tests on the staging environment first")

	got, err := r.Search("tests", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("expected at least 2 hits for 'tests', got %d", len(got))
	}
	for _, e := range got {
		if e.Content == "Coffee preferences: oat milk" {
			t.Errorf("unrelated row leaked into FTS results: %q", e.Content)
		}
	}
}

func TestSearchFTSStaysInSyncOnDeleteReinsert(t *testing.T) {
	r := setupMemoryFTSTest(t)
	id := seedMem(t, r, "Original content about apples")

	// Repository has no in-place Update; the trigger covers it
	// anyway. Simulate the equivalent by deleting + reinserting.
	if err := r.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	newID := seedMem(t, r, "Updated content about oranges")

	hits, err := r.Search("oranges", 10)
	if err != nil {
		t.Fatalf("Search oranges: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ID == newID {
			found = true
		}
	}
	if !found {
		t.Fatalf("oranges row missing from FTS results: %v", hits)
	}

	// Old term should NOT find the deleted row.
	hits, err = r.Search("apples", 10)
	if err != nil {
		t.Fatalf("Search apples: %v", err)
	}
	for _, h := range hits {
		if h.ID == id {
			t.Errorf("deleted row still surfaces in FTS")
		}
	}
}

func TestSearchFTSDroppedOnDelete(t *testing.T) {
	r := setupMemoryFTSTest(t)
	id := seedMem(t, r, "Ephemeral entry to delete")
	if err := r.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	hits, err := r.Search("Ephemeral", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits after delete, got %d", len(hits))
	}
}

func TestFTSQueryEscapesPunctuation(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello world", `"hello" "world"`},
		{"path-with-dash", `"path-with-dash"`},
		{"  spaced  out  ", `"spaced" "out"`},
		{"", `""`},
		{`he said "go"`, `"he" "said" """go"""`},
	}
	for _, c := range cases {
		got := ftsQuery(c.in)
		if got != c.want {
			t.Errorf("ftsQuery(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestSearchAdversarialInputFallsBack(t *testing.T) {
	r := setupMemoryFTSTest(t)
	seedMem(t, r, "Production-ready release notes")
	// Hyphens / slashes used to break the parser pre-quoting; now
	// they should still return the row.
	hits, err := r.Search("production-ready", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected a hit for hyphenated query")
	}
}
