package runtime

import (
	"context"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// TestEditPlan_CreatesNewVersionAndAuditEvent locks down the contract
// the desktop app's plan-review surface depends on:
//
//  1. EditPlan replaces the proposed steps with the user-supplied ones.
//  2. The plan version increments so optimistic clients see a fresh
//     resource and don't render stale state.
//  3. A plan.proposed event with `edited: true` is appended to the
//     audit chain so anyone reviewing run history sees who changed
//     what (the runtime has no dedicated plan.edited event type, but
//     the edited flag in the payload makes the user override
//     auditable).
//
// Pre-this-test, /runs/:id/plan/edit was only covered by the negative
// path in smoke_test.go (validation errors); the happy path lived
// only in e2e where it skipped on missing LLM.
func TestEditPlan_CreatesNewVersionAndAuditEvent(t *testing.T) {
	rt, database, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()
	assistantRepo := db.NewAssistantRepository(database)
	if err := assistantRepo.Create(&domain.AssistantDefinition{
		ID:        "edit-plan-assistant",
		Name:      "Edit Plan Assistant",
		Role:      "test",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assistant: %v", err)
	}

	run, err := rt.CreateRun(ctx, "test edit plan", "edit-plan-assistant")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Wait for plan_review (executeRun goroutine drives this).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := db.NewRunRepository(database).GetByID(run.ID)
		if got != nil && got.Status == domain.RunPlanReview {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, _ := db.NewRunRepository(database).GetByID(run.ID)
	if current == nil || current.Status != domain.RunPlanReview {
		t.Fatalf("run never reached plan_review, status = %v", current)
	}

	originalPlan, err := db.NewPlanRepository(database).GetByRunID(run.ID)
	if err != nil || originalPlan == nil {
		t.Fatalf("get original plan: %v", err)
	}
	originalVersion := originalPlan.Version

	// Edit: replace whatever the planner emitted with one explicit
	// llm.chat step.
	edited := []domain.StepDefinition{{
		Title:              "Reply",
		Description:        "Answer directly.",
		ExpectedTool:       "llm.chat",
		ExpectedCapability: "llm.chat",
	}}
	if err := rt.EditPlan(ctx, run.ID, edited); err != nil {
		t.Fatalf("edit plan: %v", err)
	}

	updated, err := db.NewPlanRepository(database).GetByRunID(run.ID)
	if err != nil || updated == nil {
		t.Fatalf("get updated plan: %v", err)
	}
	if len(updated.Steps) != 1 {
		t.Fatalf("step count after edit = %d, want 1", len(updated.Steps))
	}
	if got := updated.Steps[0].ExpectedTool; got != "llm.chat" {
		t.Fatalf("step tool after edit = %q, want %q", got, "llm.chat")
	}
	if updated.Version <= originalVersion {
		t.Fatalf("plan version did not increment: was %d, now %d", originalVersion, updated.Version)
	}

	// Audit: at least one plan.proposed event with edited=true must be
	// in the chain. The runtime emits plan.proposed for the initial
	// proposal AND for edits (with the edited flag set).
	events, err := db.NewEventRepository(database).ListByRun(run.ID, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var sawEdited bool
	for _, e := range events {
		if e.Type != domain.EventPlanProposed {
			continue
		}
		if v, ok := e.Payload["edited"]; ok {
			if b, _ := v.(bool); b {
				sawEdited = true
				break
			}
		}
	}
	if !sawEdited {
		t.Fatalf("no plan.proposed event with edited=true found; got %d events of all types", len(events))
	}
}

// TestEditPlan_PersistsArguments confirms the Arguments map on each
// StepDefinition survives the EditPlan round-trip. The desktop UI
// uses this to push a per-hunk-skipped diff into the persisted plan
// before approve, so the runtime applies the exact patch the user
// reviewed (filesystem.patch arguments.diff override).
func TestEditPlan_PersistsArguments(t *testing.T) {
	rt, database, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()
	assistantRepo := db.NewAssistantRepository(database)
	if err := assistantRepo.Create(&domain.AssistantDefinition{
		ID: "args-assistant", Name: "Args", Role: "test",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	run, err := rt.CreateRun(ctx, "test args", "args-assistant")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := db.NewRunRepository(database).GetByID(run.ID)
		if got != nil && got.Status == domain.RunPlanReview {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Edit with explicit Arguments (e.g. the per-hunk-skipped diff
	// the user reviewed in DiffPreview).
	wantDiff := "--- a/foo\n+++ b/foo\n@@ -1 +1 @@\n-x\n+y\n"
	edited := []domain.StepDefinition{{
		Title:        "Apply patch",
		ExpectedTool: "filesystem.patch",
		Arguments:    map[string]interface{}{"diff": wantDiff},
	}}
	if err := rt.EditPlan(ctx, run.ID, edited); err != nil {
		t.Fatalf("edit plan: %v", err)
	}

	updated, err := db.NewPlanRepository(database).GetByRunID(run.ID)
	if err != nil || updated == nil || len(updated.Steps) != 1 {
		t.Fatalf("get updated plan: %v %+v", err, updated)
	}
	gotArgs := updated.Steps[0].Arguments
	if gotArgs == nil {
		t.Fatalf("expected arguments map on persisted step, got nil")
	}
	if got, _ := gotArgs["diff"].(string); got != wantDiff {
		t.Fatalf("arguments.diff round-trip = %q, want %q", got, wantDiff)
	}
}

// TestEditPlan_RefusesIfRunNotInPlanReview guards against editing a
// plan after the user already approved it — once the run is executing,
// step rows have been mutated and editing the plan out from under
// them would corrupt state.
func TestEditPlan_RefusesIfRunNotInPlanReview(t *testing.T) {
	rt, database, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()
	assistantRepo := db.NewAssistantRepository(database)
	if err := assistantRepo.Create(&domain.AssistantDefinition{
		ID:        "guard-assistant",
		Name:      "Guard Assistant",
		Role:      "test",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create assistant: %v", err)
	}

	run, err := rt.CreateRun(ctx, "guard test", "guard-assistant")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Force the run into a non-plan_review state without going
	// through executeRun. We bypass transitionRun (which would refuse
	// from "created" → "completed") by writing to the repo directly.
	current, err := db.NewRunRepository(database).GetByID(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	current.Status = domain.RunCompleted
	current.UpdatedAt = time.Now().UTC()
	if err := db.NewRunRepository(database).Update(current); err != nil {
		t.Fatalf("force run status: %v", err)
	}

	err = rt.EditPlan(ctx, run.ID, []domain.StepDefinition{{
		Title:        "should refuse",
		ExpectedTool: "llm.chat",
	}})
	if err == nil {
		t.Fatal("EditPlan should have refused; got nil error")
	}
}
