package db

import (
	"testing"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// TestRunRepository_Search_MatchesGoalAndStepTitle covers the
// chat-list search path: a run whose goal matches OR a run whose
// step title matches both come back. Distinct on r.id so a multi-
// step match doesn't duplicate.
func TestRunRepository_Search_MatchesGoalAndStepTitle(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	seedAssistant(t, database, "asst-1")
	runRepo := NewRunRepository(database)
	stepRepo := NewStepRepository(database)

	// Run A: matches via goal.
	if err := runRepo.Create(&domain.Run{
		ID: "run-a", Goal: "Refactor auth.go", AssistantID: "asst-1",
		Status: domain.RunCompleted, PlanVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Run B: goal doesn't match; step title does.
	if err := runRepo.Create(&domain.Run{
		ID: "run-b", Goal: "Tweak headers", AssistantID: "asst-1",
		Status: domain.RunCompleted, PlanVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stepRepo.Create(&domain.Step{
		ID: "s1", RunID: "run-b", Title: "Update auth middleware",
		Status: domain.StepDone, Input: "x",
	}); err != nil {
		t.Fatal(err)
	}
	// Run C: nothing matches.
	if err := runRepo.Create(&domain.Run{
		ID: "run-c", Goal: "Bump deps", AssistantID: "asst-1",
		Status: domain.RunCompleted, PlanVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := runRepo.Search("auth", 50)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := map[string]bool{}
	for _, r := range got {
		gotIDs[r.ID] = true
	}
	if !gotIDs["run-a"] || !gotIDs["run-b"] {
		t.Fatalf("expected run-a and run-b, got %v", gotIDs)
	}
	if gotIDs["run-c"] {
		t.Fatalf("run-c should not match, got %v", gotIDs)
	}
}

// TestRunRepository_Search_EmptyQueryListsAll falls back to the
// regular listing path when the query is empty so callers can wire
// the search input straight to a single endpoint.
func TestRunRepository_Search_EmptyQueryListsAll(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()
	seedAssistant(t, database, "asst-1")
	runRepo := NewRunRepository(database)
	if err := runRepo.Create(&domain.Run{
		ID: "run-1", Goal: "anything", AssistantID: "asst-1",
		Status: domain.RunCompleted, PlanVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := runRepo.Search("", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("empty query should list all runs")
	}
}

// TestRunRepository_Search_CaseInsensitive matches the chat-list UX
// expectation that "Auth" and "auth" return the same set.
func TestRunRepository_Search_CaseInsensitive(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()
	seedAssistant(t, database, "asst-1")
	runRepo := NewRunRepository(database)
	if err := runRepo.Create(&domain.Run{
		ID: "run-1", Goal: "Refactor Auth.go", AssistantID: "asst-1",
		Status: domain.RunCompleted, PlanVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"auth", "AUTH", "Auth"} {
		got, err := runRepo.Search(q, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Fatalf("query %q should match Auth.go run", q)
		}
	}
}
