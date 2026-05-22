package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/memstore"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

func setupTestRuntimeWithMemory(t *testing.T) (*Runtime, *db.DB, *memory.EmbeddedClient, func()) {
	// Use temp file database instead of :memory: for connection stability
	tmpFile, err := os.CreateTemp("", "nomi-integration-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tmpFile.Close()

	config := db.Config{Path: tmpFile.Name()}
	database, err := db.New(config)
	if err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to create test database: %v", err)
	}

	if err := database.Migrate(); err != nil {
		database.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to run migrations: %v", err)
	}

	eventStore := db.NewEventRepository(database)
	eventBus := events.NewEventBus(eventStore)
	permEngine := permissions.NewEngine()
	approvalStore := db.NewApprovalRepository(database)
	approvalMgr := permissions.NewApprovalManager(approvalStore, eventBus)
	toolRegistry := tools.NewRegistry()
	if err := tools.RegisterCoreTools(toolRegistry); err != nil {
		t.Fatalf("Failed to register tools: %v", err)
	}
	toolExecutor := tools.NewExecutor(toolRegistry)
	memRepo := db.NewMemoryRepository(database)
	memManager := memory.NewEmbeddedClient(memRepo)

	rt := NewRuntime(database, eventBus, permEngine, approvalMgr, toolExecutor, memManager, DefaultConfig())

	cleanup := func() {
		database.Close()
		os.Remove(tmpFile.Name())
	}

	return rt, database, memManager, cleanup
}

// TestFullRunLifecycle covers: create → planning → executing → approval → completed
func TestFullRunLifecycle(t *testing.T) {
	rt, _, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()

	// Create assistant with confirm permissions (requires approval)
	assistantRepo := db.NewAssistantRepository(rt.db)
	assistant := &domain.AssistantDefinition{
		ID:               "test-assistant",
		Name:             "Test Assistant",
		Role:             "test",
		SystemPrompt:     "You are a test assistant",
		PermissionPolicy: permissions.BuildDefaultPolicy(),
		MemoryPolicy: domain.MemoryPolicy{
			Enabled: true,
			Scope:   "workspace",
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Create a run
	run, err := rt.CreateRun(ctx, "echo test", assistant.ID)
	if err != nil {
		t.Fatalf("Failed to create run: %v", err)
	}

	if run.Status != domain.RunCreated {
		t.Errorf("Initial status should be 'created', got '%s'", run.Status)
	}

	// Wait for execution to reach plan review
	time.Sleep(500 * time.Millisecond)

	// Verify run is in plan review
	updatedRun, steps, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if updatedRun.Status != domain.RunPlanReview {
		t.Errorf("Expected status 'plan_review', got '%s'", updatedRun.Status)
	}

	if len(steps) == 0 {
		t.Fatal("Expected at least one step")
	}
	if steps[0].Status != domain.StepPending {
		t.Errorf("Expected step status 'pending', got '%s'", steps[0].Status)
	}

	// Approve the plan to start execution
	if err := rt.ApprovePlan(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve plan: %v", err)
	}

	// Wait for execution to reach approval
	time.Sleep(1 * time.Second)

	// Verify run is awaiting approval
	updatedRun, steps, _, err = rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if updatedRun.Status != domain.RunAwaitingApproval {
		t.Errorf("Expected status 'awaiting_approval', got '%s'", updatedRun.Status)
	}

	// Wait for execution to reach approval
	time.Sleep(500 * time.Millisecond)

	// Verify run is awaiting approval
	updatedRun, steps, _, err = rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if updatedRun.Status != domain.RunAwaitingApproval {
		t.Errorf("Expected status 'awaiting_approval', got '%s'", updatedRun.Status)
	}

	// Verify step is blocked
	if len(steps) == 0 {
		t.Fatal("Expected at least one step")
	}
	if steps[0].Status != domain.StepBlocked {
		t.Errorf("Expected step status 'blocked', got '%s'", steps[0].Status)
	}

	// Verify approval was created
	approvals, err := rt.approvalMgr.GetByRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get approvals: %v", err)
	}
	if len(approvals) == 0 {
		t.Fatal("Expected at least one approval request")
	}
	if approvals[0].Status != permissions.ApprovalPending {
		t.Errorf("Expected approval status 'pending', got '%s'", approvals[0].Status)
	}

	// Approve the run
	if err := rt.ApproveRun(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve run: %v", err)
	}

	// Wait for execution to complete
	time.Sleep(500 * time.Millisecond)

	// Verify run completed
	completedRun, completedSteps, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run after approval: %v", err)
	}

	if completedRun.Status != domain.RunCompleted {
		t.Errorf("Expected status 'completed', got '%s'", completedRun.Status)
	}

	if len(completedSteps) == 0 {
		t.Fatal("Expected steps after completion")
	}

	if completedSteps[0].Status != domain.StepDone {
		t.Errorf("Expected step status 'done', got '%s'", completedSteps[0].Status)
	}

	// Verify memory was stored
	memories, err := rt.memClient.Retrieve(ctx, memstore.LocalWorkspace(), memstore.Query{Limit: 10})
	if err != nil {
		t.Fatalf("Failed to list memories: %v", err)
	}
	if len(memories) == 0 {
		t.Error("Expected memory to be stored after run completion")
	}
}

// TestApprovalDeny covers: create → approval → deny → failed
func TestApprovalDeny(t *testing.T) {
	rt, _, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()

	// Create assistant with confirm permissions
	assistantRepo := db.NewAssistantRepository(rt.db)
	assistant := &domain.AssistantDefinition{
		ID:               "test-deny-assistant",
		Name:             "Deny Test",
		Role:             "test",
		SystemPrompt:     "Test",
		PermissionPolicy: permissions.BuildDefaultPolicy(),
		CreatedAt:        time.Now().UTC(),
	}
	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Create run
	run, err := rt.CreateRun(ctx, "echo denied", assistant.ID)
	if err != nil {
		t.Fatalf("Failed to create run: %v", err)
	}

	// Wait for plan review
	time.Sleep(500 * time.Millisecond)

	// Approve plan
	if err := rt.ApprovePlan(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve plan: %v", err)
	}

	// Wait for approval
	time.Sleep(1 * time.Second)

	// Deny the approval
	approvals, err := rt.approvalMgr.GetByRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get approvals: %v", err)
	}
	if len(approvals) == 0 {
		t.Fatal("Expected approval request")
	}

	if err := rt.approvalMgr.Resolve(ctx, approvals[0].ID, false); err != nil {
		t.Fatalf("Failed to deny approval: %v", err)
	}

	// Wait for failure
	time.Sleep(500 * time.Millisecond)

	// Verify run failed
	failedRun, _, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if failedRun.Status != domain.RunFailed {
		t.Errorf("Expected status 'failed', got '%s'", failedRun.Status)
	}
}

// TestRunRetry covers: create → fail → retry → complete
func TestRunRetry(t *testing.T) {
	rt, _, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()

	// Create assistant with permissive policy (no approval needed)
	assistantRepo := db.NewAssistantRepository(rt.db)
	assistant := &domain.AssistantDefinition{
		ID:               "test-retry-assistant",
		Name:             "Retry Test",
		Role:             "test",
		SystemPrompt:     "Test",
		PermissionPolicy: permissions.BuildPermissivePolicy(),
		CreatedAt:        time.Now().UTC(),
	}
	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Create run
	run, err := rt.CreateRun(ctx, "echo retry-test", assistant.ID)
	if err != nil {
		t.Fatalf("Failed to create run: %v", err)
	}

	// Wait for plan review
	time.Sleep(500 * time.Millisecond)

	// Approve plan
	if err := rt.ApprovePlan(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve plan: %v", err)
	}

	// Wait for completion
	time.Sleep(1 * time.Second)

	// Verify completed
	completedRun, _, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if completedRun.Status != domain.RunCompleted {
		t.Errorf("Expected status 'completed', got '%s'", completedRun.Status)
	}

	// Retry should succeed because run is in terminal state (completed)
	if err := rt.RetryRun(ctx, run.ID); err != nil {
		t.Errorf("Expected retry to succeed for completed run: %v", err)
	}

	// Wait for retry to reach plan review
	time.Sleep(500 * time.Millisecond)

	// Applan plan for retry
	if err := rt.ApprovePlan(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve plan after retry: %v", err)
	}

	// Wait for retry to complete
	time.Sleep(1 * time.Second)

	// Verify retry completed
	retriedRun, _, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run after retry: %v", err)
	}

	if retriedRun.Status != domain.RunCompleted {
		t.Errorf("Expected status 'completed' after retry, got '%s'", retriedRun.Status)
	}

	if retriedRun.PlanVersion != 2 {
		t.Errorf("Expected plan version 2 after retry, got %d", retriedRun.PlanVersion)
	}
}

// TestEventEmission verifies events are published during run lifecycle
func TestEventEmission(t *testing.T) {
	rt, _, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()

	// Subscribe to events
	sub := rt.eventBus.Subscribe(events.EventFilter{})
	defer sub.Unsubscribe()

	// Create assistant
	assistantRepo := db.NewAssistantRepository(rt.db)
	assistant := &domain.AssistantDefinition{
		ID:               "test-event-assistant",
		Name:             "Event Test",
		Role:             "test",
		SystemPrompt:     "Test",
		PermissionPolicy: permissions.BuildPermissivePolicy(),
		CreatedAt:        time.Now().UTC(),
	}
	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Create run
	run, err := rt.CreateRun(ctx, "echo events", assistant.ID)
	if err != nil {
		t.Fatalf("Failed to create run: %v", err)
	}

	// Wait for plan review and approve
	time.Sleep(500 * time.Millisecond)
	if err := rt.ApprovePlan(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve plan: %v", err)
	}

	// Collect events with timeout
	eventTypes := make(map[domain.EventType]bool)
	timeout := time.After(2 * time.Second)
	done := false

	for !done {
		select {
		case event := <-sub.Events():
			eventTypes[event.Type] = true
		case <-timeout:
			done = true
		}
	}

	// Verify expected events were emitted
	expectedEvents := []domain.EventType{
		domain.EventRunCreated,
		domain.EventPlanProposed,
		domain.EventStepStarted,
		domain.EventStepCompleted,
		domain.EventRunCompleted,
	}

	for _, eventType := range expectedEvents {
		if !eventTypes[eventType] {
			t.Errorf("Expected event '%s' to be emitted", eventType)
		}
	}
}

// TestFolderContextLoading verifies that folder contexts are loaded into run steps
func TestFolderContextLoading(t *testing.T) {
	rt, _, _, cleanup := setupTestRuntimeWithMemory(t)
	defer cleanup()

	ctx := context.Background()

	// Create a temporary folder with some files
	tmpDir, err := os.MkdirTemp("", "nomi-context-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create some files
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test"), 0644); err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(tmpDir, "src"), 0755); err != nil {
		t.Fatalf("Failed to create dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "src", "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	// Create assistant with folder context
	assistantRepo := db.NewAssistantRepository(rt.db)
	assistant := &domain.AssistantDefinition{
		ID:               "test-context-assistant",
		Name:             "Context Test",
		Role:             "test",
		SystemPrompt:     "Test",
		PermissionPolicy: permissions.BuildPermissivePolicy(),
		Contexts: []domain.ContextAttachment{
			{Type: "folder", Path: tmpDir},
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Create run
	run, err := rt.CreateRun(ctx, "list files", assistant.ID)
	if err != nil {
		t.Fatalf("Failed to create run: %v", err)
	}

	// Wait for plan review and approve
	time.Sleep(500 * time.Millisecond)
	if err := rt.ApprovePlan(ctx, run.ID); err != nil {
		t.Fatalf("Failed to approve plan: %v", err)
	}

	// Wait for completion
	time.Sleep(1 * time.Second)

	// Verify run completed
	completedRun, steps, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if completedRun.Status != domain.RunCompleted {
		t.Errorf("Expected status 'completed', got '%s'", completedRun.Status)
	}

	if len(steps) == 0 {
		t.Fatal("Expected at least one step")
	}

	// Verify step input contains the folder context
	step := steps[0]
	if !strings.Contains(step.Input, "Attached context:") {
		t.Errorf("Expected step input to contain 'Attached context:', got:\n%s", step.Input)
	}
	if !strings.Contains(step.Input, filepath.Base(tmpDir)) {
		t.Errorf("Expected step input to contain folder name '%s', got:\n%s", filepath.Base(tmpDir), step.Input)
	}
	if !strings.Contains(step.Input, "README.md") {
		t.Errorf("Expected step input to contain 'README.md', got:\n%s", step.Input)
	}
	if !strings.Contains(step.Input, "main.go") {
		t.Errorf("Expected step input to contain 'main.go', got:\n%s", step.Input)
	}
}
