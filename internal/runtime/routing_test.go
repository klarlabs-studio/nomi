package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// TestDetermineToolRoutesViaStepDefinition asserts feature #34's core
// promise: a step whose StepDefinition declares ExpectedTool=X is routed
// to X, not to the legacy command.exec default.
func TestDetermineToolRoutesViaStepDefinition(t *testing.T) {
	dir := t.TempDir()

	database, err := db.New(db.Config{Path: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalMgr := permissions.NewApprovalManager(db.NewApprovalRepository(database), bus)
	toolReg := tools.NewRegistry()
	_ = tools.RegisterCoreTools(toolReg)
	toolExec := tools.NewExecutor(toolReg)
	memMgr := memory.NewEmbeddedClient(db.NewMemoryRepository(database))

	rt := NewRuntime(database, bus, permEngine, approvalMgr, toolExec, memMgr, DefaultConfig())
	defer rt.Shutdown()

	// Seed a plan + step_definition with ExpectedTool=filesystem.read.
	plan := &domain.Plan{
		ID:        "plan-1",
		RunID:     "run-1",
		Version:   1,
		CreatedAt: time.Now().UTC(),
	}
	def := domain.StepDefinition{
		ID:                 "def-1",
		PlanID:             plan.ID,
		Title:              "Read the README",
		ExpectedTool:       "filesystem.read",
		ExpectedCapability: "filesystem.read",
		Order:              0,
		CreatedAt:          time.Now().UTC(),
	}
	plan.Steps = []domain.StepDefinition{def}

	// runs has an FK to assistants; seed the assistant first.
	if err := db.NewAssistantRepository(database).Create(&domain.AssistantDefinition{
		ID: "a-1", Name: "test", Role: "dev", SystemPrompt: "t",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.NewRunRepository(database).Create(&domain.Run{
		ID:          "run-1",
		Goal:        "read docs",
		AssistantID: "a-1",
		Status:      domain.RunCreated,
		PlanVersion: 1,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.planRepo.Create(plan); err != nil {
		t.Fatal(err)
	}

	defID := def.ID
	step := &domain.Step{
		ID:               "step-1",
		RunID:            "run-1",
		StepDefinitionID: &defID,
		Title:            def.Title,
		Status:           domain.StepPending,
		Input:            "README.md",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	got := rt.determineTool(step)
	if got != "filesystem.read" {
		t.Fatalf("determineTool routed to %q, expected filesystem.read", got)
	}
}

// TestDetermineToolFallsBackToCommandExecForLegacySteps asserts the
// backward-compat path: a step without a StepDefinition (legacy row
// written before #34) still routes to command.exec.
func TestDetermineToolFallsBackToCommandExecForLegacySteps(t *testing.T) {
	dir := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	bus := events.NewEventBus(db.NewEventRepository(database))
	rt := NewRuntime(
		database, bus,
		permissions.NewEngine(),
		permissions.NewApprovalManager(db.NewApprovalRepository(database), bus),
		tools.NewExecutor(tools.NewRegistry()),
		memory.NewEmbeddedClient(db.NewMemoryRepository(database)),
		DefaultConfig(),
	)
	defer rt.Shutdown()

	// Step without a StepDefinitionID.
	step := &domain.Step{ID: "s", RunID: "r", Title: "legacy"}
	if got := rt.determineTool(step); got != "command.exec" {
		t.Fatalf("fallback: got %q, want command.exec", got)
	}

	// Step with a non-existent StepDefinitionID.
	ghost := "not-a-real-def-id"
	step.StepDefinitionID = &ghost
	if got := rt.determineTool(step); got != "command.exec" {
		t.Fatalf("unknown-def fallback: got %q, want command.exec", got)
	}
}
