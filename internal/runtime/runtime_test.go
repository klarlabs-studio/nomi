package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/memory"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
	"go.klarlabs.de/nomi/pkg/statekit"
)

func setupTestRuntime(t *testing.T) (*Runtime, *db.DB, func()) {
	// Create temp database file (shared across connections)
	tmpFile, err := os.CreateTemp("", "nomi-test-*.db")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()

	config := db.Config{Path: tmpFile.Name()}
	database, err := db.New(config)
	if err != nil {
		_ = os.Remove(tmpFile.Name())
		t.Fatalf("Failed to create test database: %v", err)
	}

	// Run migrations
	if err := database.Migrate(); err != nil {
		_ = database.Close()
		_ = os.Remove(tmpFile.Name())
		t.Fatalf("Failed to run migrations: %v", err)
	}

	// Setup components
	eventStore := db.NewEventRepository(database)
	eventBus := events.NewEventBus(eventStore)
	permEngine := permissions.NewEngine()
	approvalStore := db.NewApprovalRepository(database)
	approvalMgr := permissions.NewApprovalManager(approvalStore, eventBus)
	toolRegistry := tools.NewRegistry()
	if err := tools.RegisterCoreTools(toolRegistry); err != nil {
		_ = database.Close()
		_ = os.Remove(tmpFile.Name())
		t.Fatalf("Failed to register tools: %v", err)
	}
	toolExecutor := tools.NewExecutor(toolRegistry)
	memRepo := db.NewMemoryRepository(database)
	memManager := memory.NewEmbeddedClient(memRepo)
	rt := NewRuntime(database, eventBus, permEngine, approvalMgr, toolExecutor, memManager, DefaultConfig())

	cleanup := func() {
		_ = database.Close()
		_ = os.Remove(tmpFile.Name())
	}

	return rt, database, cleanup
}

func TestCreateRun(t *testing.T) {
	rt, _, cleanup := setupTestRuntime(t)
	defer cleanup()

	ctx := context.Background()

	// Create an assistant first
	assistantRepo := db.NewAssistantRepository(rt.db)
	assistant := &domain.AssistantDefinition{
		ID:               "test-assistant",
		Name:             "Test Assistant",
		Role:             "test",
		SystemPrompt:     "You are a test assistant",
		PermissionPolicy: permissions.BuildPermissivePolicy(),
		CreatedAt:        time.Now().UTC(),
	}
	if err := assistantRepo.Create(assistant); err != nil {
		t.Fatalf("Failed to create assistant: %v", err)
	}

	// Create a run
	run, err := rt.CreateRun(ctx, "Test goal", assistant.ID)
	if err != nil {
		t.Fatalf("Failed to create run: %v", err)
	}

	if run.ID == "" {
		t.Error("Run ID should not be empty")
	}
	if run.Goal != "Test goal" {
		t.Errorf("Expected goal 'Test goal', got '%s'", run.Goal)
	}
	if run.Status != domain.RunCreated {
		t.Errorf("Expected status 'created', got '%s'", run.Status)
	}

	// Give the run time to execute asynchronously
	time.Sleep(100 * time.Millisecond)

	// Verify run was persisted
	retrievedRun, steps, _, err := rt.GetRun(run.ID)
	if err != nil {
		t.Fatalf("Failed to get run: %v", err)
	}

	if retrievedRun.ID != run.ID {
		t.Error("Retrieved run ID mismatch")
	}

	// Should have at least one step
	if len(steps) == 0 {
		t.Error("Expected at least one step")
	}
}

func TestPermissionEvaluation(t *testing.T) {
	permEngine := permissions.NewEngine()

	policy := permissions.BuildDefaultPolicy()

	tests := []struct {
		capability string
		expected   domain.PermissionMode
	}{
		{"filesystem.read", domain.PermissionAllow},
		{"filesystem.write", domain.PermissionConfirm},
		{"command.exec", domain.PermissionConfirm},
		{"network.http", domain.PermissionDeny},
	}

	for _, tt := range tests {
		result := permEngine.Evaluate(policy, tt.capability)
		if result != tt.expected {
			t.Errorf("Evaluate(%s) = %v, expected %v", tt.capability, result, tt.expected)
		}
	}
}

func TestToolExecution(t *testing.T) {
	registry := tools.NewRegistry()
	if err := tools.RegisterCoreTools(registry); err != nil {
		t.Fatalf("Failed to register tools: %v", err)
	}

	executor := tools.NewExecutor(registry)
	ctx := context.Background()

	// filesystem.read and filesystem.write are sandboxed to a workspace root.
	// Create a dedicated temp dir that will serve as the root for this test.
	workspaceRoot, err := os.MkdirTemp("", "nomi-test-root-")
	if err != nil {
		t.Fatalf("Failed to create workspace root: %v", err)
	}
	defer func() { _ = os.RemoveAll(workspaceRoot) }()

	testFile := filepath.Join(workspaceRoot, "hello.txt")
	testContent := "Hello, Nomi!"
	if err := os.WriteFile(testFile, []byte(testContent), 0o644); err != nil {
		t.Fatalf("Failed to seed test file: %v", err)
	}

	// filesystem.read
	result := executor.Execute(ctx, "filesystem.read", map[string]interface{}{
		"workspace_root": workspaceRoot,
		"path":           "hello.txt",
	})
	if !result.Success {
		t.Errorf("filesystem.read failed: %s", result.Error)
	}
	if result.Output != nil {
		if content, ok := result.Output["content"].(string); !ok || content != testContent {
			t.Errorf("Expected content '%s', got '%v'", testContent, result.Output["content"])
		}
	}

	// filesystem.write
	result = executor.Execute(ctx, "filesystem.write", map[string]interface{}{
		"workspace_root": workspaceRoot,
		"path":           "written.txt",
		"content":        testContent,
	})
	if !result.Success {
		t.Errorf("filesystem.write failed: %s", result.Error)
	}

	// Path escape is refused.
	escape := executor.Execute(ctx, "filesystem.read", map[string]interface{}{
		"workspace_root": workspaceRoot,
		"path":           "../../../etc/passwd",
	})
	if escape.Success {
		t.Errorf("filesystem.read should refuse paths escaping the workspace root")
	}

	// Missing workspace_root is refused.
	noRoot := executor.Execute(ctx, "filesystem.read", map[string]interface{}{
		"path": testFile,
	})
	if noRoot.Success {
		t.Errorf("filesystem.read should refuse calls without workspace_root")
	}
}

func TestStateMachine(t *testing.T) {
	// Test Run state machine
	runSM := statekit.NewRunStateMachine()

	if runSM.Current() != domain.RunCreated {
		t.Errorf("Initial state should be 'created', got '%s'", runSM.Current())
	}

	if !runSM.CanTransition(domain.RunPlanning) {
		t.Error("Should be able to transition from created to planning")
	}

	if runSM.CanTransition(domain.RunCompleted) {
		t.Error("Should not be able to transition from created to completed")
	}

	// Test Step state machine
	stepSM := statekit.NewStepStateMachine()

	if stepSM.Current() != domain.StepPending {
		t.Errorf("Initial state should be 'pending', got '%s'", stepSM.Current())
	}

	if !stepSM.CanTransition(domain.StepReady) {
		t.Error("Should be able to transition from pending to ready")
	}
}
