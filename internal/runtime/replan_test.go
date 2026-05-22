package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/llm"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// TestReplan_BudgetEnforced confirms a planner that keeps proposing
// plans cannot replan more than MaxReplansPerRun times. The third
// call must return the budget-exhausted error so the executor falls
// back to failRun and a human can step in.
func TestReplan_BudgetEnforced(t *testing.T) {
	rt, database := newReplanRuntime(t, simplePlanResponder)
	ctx := context.Background()

	if err := db.NewAssistantRepository(database).Create(replanTestAssistant()); err != nil {
		t.Fatal(err)
	}
	run, err := rt.CreateRun(ctx, "test budget", "a")
	if err != nil {
		t.Fatal(err)
	}
	waitForPlanReview(t, database, run.ID)

	for i := 0; i < MaxReplansPerRun; i++ {
		if _, err := rt.Replan(ctx, run, nil, "boom"); err != nil {
			t.Fatalf("replan %d: unexpected error %v", i, err)
		}
	}
	if _, err := rt.Replan(ctx, run, nil, "boom"); err == nil {
		t.Fatal("expected budget-exhausted error on call 3")
	}
}

// TestReplan_PassesPriorAttemptsToPlanner asserts the prior step
// outputs and the failure message reach the LLM via a trusted=false
// previous_attempts block. Without this, the replan is just a redo.
func TestReplan_PassesPriorAttemptsToPlanner(t *testing.T) {
	var seenBody string
	rt, database := newReplanRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "planning assistant for Nomi") {
			seenBody = string(body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"{\"steps\":[{\"title\":\"Reply\",\"description\":\"Answer.\",\"tool\":\"llm.chat\",\"arguments\":{\"prompt\":\"hi\"}}]}"}}]}`))
	})

	ctx := context.Background()
	if err := db.NewAssistantRepository(database).Create(replanTestAssistant()); err != nil {
		t.Fatal(err)
	}
	run, err := rt.CreateRun(ctx, "investigate failure", "a")
	if err != nil {
		t.Fatal(err)
	}
	waitForPlanReview(t, database, run.ID)
	current, _ := db.NewRunRepository(database).GetByID(run.ID)

	failed := &domain.Step{
		ID:     "00000000-0000-0000-0000-000000000000",
		Title:  "Run tests",
		Output: "stderr from go test goes here",
	}
	if _, err := rt.Replan(ctx, current, failed, "exit code 1: TestFoo failed"); err != nil {
		t.Fatalf("replan: %v", err)
	}
	if !strings.Contains(seenBody, "previous_attempts") {
		t.Fatalf("planner request did not include previous_attempts block: %q", seenBody)
	}
	if !strings.Contains(seenBody, "exit code 1: TestFoo failed") {
		t.Fatalf("planner request did not include failure message: %q", seenBody)
	}
}

// TestManualReplan_RefusesNonTerminalRun is the safety check on the
// /runs/:id/replan API endpoint: the user-facing CTA only fires on
// failed runs, but a buggy client could POST against an
// in-flight run. ManualReplan must refuse so we don't end up with
// two executor goroutines on the same run.
func TestManualReplan_RefusesNonTerminalRun(t *testing.T) {
	rt, database := newReplanRuntime(t, simplePlanResponder)
	ctx := context.Background()
	if err := db.NewAssistantRepository(database).Create(replanTestAssistant()); err != nil {
		t.Fatal(err)
	}
	run, err := rt.CreateRun(ctx, "test", "a")
	if err != nil {
		t.Fatal(err)
	}
	waitForPlanReview(t, database, run.ID)

	// Run is in plan_review (non-terminal). ManualReplan must refuse.
	if _, err := rt.ManualReplan(ctx, run.ID); err == nil {
		t.Fatal("ManualReplan should refuse a non-terminal run")
	}
}

// simplePlanResponder is the default fake-LLM handler used by replan
// tests that don't care about the prompt body shape — just returns a
// valid one-step llm.chat plan so the runtime accepts it.
func simplePlanResponder(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"{\"steps\":[{\"title\":\"Reply\",\"description\":\"Answer.\",\"tool\":\"llm.chat\",\"arguments\":{\"prompt\":\"hi\"}}]}"}}]}`))
}

// newReplanRuntime spins a Runtime with a real DB, a registered
// llm.chat tool, and a default provider profile pointed at the given
// httptest handler. Mirrors the manual setup in llm_integration_test.go;
// kept local to this file so the unit-test package isn't entangled
// with that file's specific fixtures.
func newReplanRuntime(t *testing.T, h http.HandlerFunc) (*Runtime, *db.DB) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	providerRepo := db.NewProviderProfileRepository(database)
	if err := providerRepo.Create(&domain.ProviderProfile{
		ID: "p", Name: "F", Type: "remote", Endpoint: srv.URL,
		ModelIDs: []string{"m"}, Enabled: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	settings := db.NewGlobalSettingsRepository(database)
	if err := settings.SetLLMDefault("p", "m"); err != nil {
		t.Fatal(err)
	}
	resolver := llm.NewResolver(providerRepo, settings, &replanInMemoryStore{data: map[string]string{}})

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalMgr := permissions.NewApprovalManager(db.NewApprovalRepository(database), bus)
	toolReg := tools.NewRegistry()
	_ = tools.RegisterCoreTools(toolReg)
	_ = toolReg.Register(tools.NewLLMChatTool(resolver))
	memMgr := memory.NewTestClient(t)

	rt := NewRuntime(database, bus, permEngine, approvalMgr, tools.NewExecutor(toolReg), memMgr, DefaultConfig())
	rt.SetLLMResolver(resolver)
	t.Cleanup(rt.Shutdown)
	return rt, database
}

func replanTestAssistant() *domain.AssistantDefinition {
	return &domain.AssistantDefinition{
		ID: "a", Name: "A", Role: "test",
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{{Capability: "llm.chat", Mode: domain.PermissionAllow}},
		},
		Capabilities: []string{"llm.chat"},
		CreatedAt:    time.Now().UTC(),
	}
}

func waitForPlanReview(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := db.NewRunRepository(database).GetByID(runID)
		if got != nil && got.Status == domain.RunPlanReview {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run never reached plan_review")
}

type replanInMemoryStore struct{ data map[string]string }

func (s *replanInMemoryStore) Put(k, v string) error { s.data[k] = v; return nil }
func (s *replanInMemoryStore) Get(k string) (string, error) {
	v, ok := s.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (s *replanInMemoryStore) Delete(k string) error { delete(s.data, k); return nil }
