package db

import (
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

func seedRunForChainTest(t *testing.T, database *DB, runID, assistantID string) {
	t.Helper()
	seedAssistant(t, database, assistantID)
	if _, err := database.Exec(
		`INSERT INTO runs (id, goal, assistant_id, status) VALUES (?, ?, ?, 'created')`,
		runID, "test", assistantID,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// TestEventRepository_VerifyChain_OK exercises the happy path: append a
// few events through the public API, then walk the chain and confirm
// every entry's hash matches the recomputed value.
func TestEventRepository_VerifyChain_OK(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	seedRunForChainTest(t, database, "run-1", "asst-1")
	repo := NewEventRepository(database)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		evt := &domain.Event{
			ID:        "ev-" + string(rune('a'+i)),
			Type:      domain.EventRunCreated,
			RunID:     "run-1",
			Payload:   map[string]interface{}{"i": i},
			Timestamp: now.Add(time.Duration(i) * time.Millisecond),
		}
		if err := repo.Create(evt); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	res, err := repo.VerifyChain()
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK chain, got: %+v", res)
	}
	if res.Count != 5 {
		t.Fatalf("expected 5 verified entries, got %d", res.Count)
	}
}

// TestEventRepository_VerifyChain_DetectsTamperedPayload mutates a row's
// payload directly and confirms VerifyChain points at the offending
// entry. This is the marketing-claim test: README says "hash-chained
// audit log"; this test makes the claim true.
func TestEventRepository_VerifyChain_DetectsTamperedPayload(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	seedRunForChainTest(t, database, "run-1", "asst-1")
	repo := NewEventRepository(database)
	now := time.Now().UTC()
	ids := []string{"ev-1", "ev-2", "ev-3"}
	for i, id := range ids {
		_ = repo.Create(&domain.Event{
			ID:        id,
			Type:      domain.EventRunCreated,
			RunID:     "run-1",
			Payload:   map[string]interface{}{"step": i},
			Timestamp: now.Add(time.Duration(i) * time.Millisecond),
		})
	}

	// Tamper: rewrite ev-2's payload behind the chain's back. The
	// stored entry_hash and prev_hash are untouched, but the recomputed
	// hash will diverge — verify catches it.
	if _, err := database.Exec(
		`UPDATE events SET payload = ? WHERE id = ?`,
		`{"step":99,"injected":true}`, "ev-2",
	); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := repo.VerifyChain()
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if res.OK {
		t.Fatalf("verify should have flagged the tampered row, got OK")
	}
	if res.FirstBadEventID != "ev-2" {
		t.Fatalf("expected first_bad=ev-2, got %q", res.FirstBadEventID)
	}
}
