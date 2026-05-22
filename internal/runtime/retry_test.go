package runtime

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// flakyTool fails with a transient error N times, then succeeds. Used to
// assert the retry loop actually re-invokes the executor.
type flakyTool struct {
	fails        int
	calls        atomic.Int32
	failureError string
}

func (f *flakyTool) Name() string       { return "flaky" }
func (f *flakyTool) Capability() string { return "flaky" }
func (f *flakyTool) Execute(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	n := f.calls.Add(1)
	if int(n) <= f.fails {
		return nil, errText(f.failureError)
	}
	return map[string]interface{}{"output": "ok"}, nil
}

type errText string

func (e errText) Error() string { return string(e) }

func TestIsTransientFailureClassification(t *testing.T) {
	transient := []string{
		"dial tcp: i/o timeout",
		"connection refused by upstream",
		"429 Too Many Requests",
		"503 Service Unavailable",
		"temporary failure in name resolution",
	}
	for _, m := range transient {
		if !isTransientFailure(m) {
			t.Errorf("expected transient: %q", m)
		}
	}

	deterministic := []string{
		"capability not declared: command.exec",
		"policy denies capability: command.exec",
		"binary \"rm\" is not in the allowed_binaries list",
		"path escapes workspace root",
		"command contains shell metacharacter \";\"",
	}
	for _, m := range deterministic {
		if isTransientFailure(m) {
			t.Errorf("expected deterministic: %q", m)
		}
	}
}

// retryHarness stands up a Runtime with a real SQLite database and a
// swappable tool registry. Returns helpers the tests use to seed rows.
type retryHarness struct {
	rt *Runtime
	db *db.DB
}

func newRetryHarness(t *testing.T, maxRetries int) *retryHarness {
	t.Helper()
	dir := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalMgr := permissions.NewApprovalManager(db.NewApprovalRepository(database), bus)
	memMgr := memory.NewEmbeddedClient(db.NewMemoryRepository(database))

	cfg := DefaultConfig()
	cfg.MaxRetries = maxRetries
	rt := NewRuntime(database, bus, permEngine, approvalMgr, tools.NewExecutor(tools.NewRegistry()), memMgr, cfg)
	t.Cleanup(rt.Shutdown)
	return &retryHarness{rt: rt, db: database}
}

// seedRunAndStep writes enough rows that invokeWithRetry's
// stepRepo.Update succeeds on each attempt.
func (h *retryHarness) seedRunAndStep(t *testing.T, runID, stepID string) *domain.Step {
	t.Helper()
	if err := db.NewAssistantRepository(h.db).Create(&domain.AssistantDefinition{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := db.NewRunRepository(h.db).Create(&domain.Run{
		ID: runID, Goal: "g", AssistantID: "a",
		Status: domain.RunCreated, PlanVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	step := &domain.Step{ID: stepID, RunID: runID, Title: "t", Status: domain.StepRunning}
	if err := db.NewStepRepository(h.db).Create(step); err != nil {
		t.Fatal(err)
	}
	return step
}

func (h *retryHarness) setExecutor(reg *tools.Registry) {
	h.rt.toolExecutor = tools.NewExecutor(reg)
}

// TestRetryLoopReinvokesOnTransientFailure asserts invokeWithRetry picks
// the flakyTool up the second (or third) time and returns success.
func TestRetryLoopReinvokesOnTransientFailure(t *testing.T) {
	h := newRetryHarness(t, 3)
	flaky := &flakyTool{fails: 2, failureError: "dial tcp: i/o timeout"}
	reg := tools.NewRegistry()
	if err := reg.Register(flaky); err != nil {
		t.Fatal(err)
	}
	h.setExecutor(reg)

	step := h.seedRunAndStep(t, "r1", "s1")
	result := h.rt.invokeWithRetry(context.Background(), &domain.Run{ID: "r1"}, step, "flaky", nil, "flaky")
	if !result.Success {
		t.Fatalf("expected success after retries, got error: %s", result.Error)
	}
	if got := flaky.calls.Load(); got != 3 {
		t.Fatalf("flakyTool called %d times, expected 3", got)
	}
	if step.RetryCount != 2 {
		t.Fatalf("step.RetryCount = %d, expected 2", step.RetryCount)
	}
}

// TestRetryLoopGivesUpOnDeterministicFailure asserts that a non-transient
// error stops immediately without consuming the retry budget.
func TestRetryLoopGivesUpOnDeterministicFailure(t *testing.T) {
	h := newRetryHarness(t, 3)
	flaky := &flakyTool{fails: 99, failureError: "policy denies capability: command.exec"}
	reg := tools.NewRegistry()
	_ = reg.Register(flaky)
	h.setExecutor(reg)

	step := h.seedRunAndStep(t, "r2", "s2")
	result := h.rt.invokeWithRetry(context.Background(), &domain.Run{ID: "r2"}, step, "flaky", nil, "flaky")
	if result.Success {
		t.Fatal("expected failure")
	}
	if got := flaky.calls.Load(); got != 1 {
		t.Fatalf("flakyTool called %d times, expected 1 (no retries on deterministic failure)", got)
	}
	if step.RetryCount != 0 {
		t.Fatalf("step.RetryCount = %d, expected 0", step.RetryCount)
	}
}

// TestRetryLoopExhaustsBudget asserts that after maxRetries failures, the
// loop gives up and returns the last error.
func TestRetryLoopExhaustsBudget(t *testing.T) {
	h := newRetryHarness(t, 2)
	flaky := &flakyTool{fails: 99, failureError: "connection refused"}
	reg := tools.NewRegistry()
	_ = reg.Register(flaky)
	h.setExecutor(reg)

	step := h.seedRunAndStep(t, "r3", "s3")
	result := h.rt.invokeWithRetry(context.Background(), &domain.Run{ID: "r3"}, step, "flaky", nil, "flaky")
	if result.Success {
		t.Fatal("expected failure after exhausting retries")
	}
	if got := flaky.calls.Load(); got != 3 {
		t.Fatalf("flakyTool called %d times, expected 3 (1 + 2 retries)", got)
	}
	if step.RetryCount != 2 {
		t.Fatalf("step.RetryCount = %d, expected 2", step.RetryCount)
	}
}
