package evals

import (
	"encoding/json"
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
	"github.com/felixgeelhaar/nomi/internal/runtime"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// adversarialCase exercises one model-misbehavior pattern. The
// expected resilience: the runtime must either (a) recover via the
// planner's self-repair retry loop OR (b) leave the run terminal in
// a clean state — never crash, never apply a bogus plan, never let
// an unknown tool name reach the executor.
type adversarialCase struct {
	name        string
	plannerJSON string
	// wantPlanReached is true if the runtime is expected to produce a
	// usable plan after self-repair (the model emits prose around
	// JSON; parsePlannerResponse extracts it). False if the case is a
	// hard rejection where the run should never reach plan_review.
	wantPlanReached bool
}

// adversarialCases — keep small + sharp; each case documents one
// resilience invariant.
var adversarialCases = []adversarialCase{
	{
		name: "markdown-fenced JSON",
		plannerJSON: "```json\n" + `{"steps":[{"title":"Reply","description":"hi","tool":"llm.chat","arguments":{"prompt":"hi"}}]}` + "\n```",
		// parsePlannerResponse strips fences; should reach plan_review.
		wantPlanReached: true,
	},
	{
		name:            "prose preamble around JSON",
		plannerJSON:     `Sure, here is the plan:\n{"steps":[{"title":"Reply","description":"hi","tool":"llm.chat","arguments":{"prompt":"hi"}}]}\nLet me know if you want changes!`,
		wantPlanReached: true,
	},
	{
		name:            "unknown tool name",
		plannerJSON:     `{"steps":[{"title":"Browse","description":"open URL","tool":"web.browse","arguments":{"url":"https://x.com"}}]}`,
		wantPlanReached: false,
	},
}

// TestPlannerAdversarialResilience drives each case through the real
// runtime + plan-review pipeline against an httptest fake LLM. The
// test passes when the planner either accepts (for recoverable
// malformations) or rejects (for hard violations) — never when a
// bad plan reaches execution.
func TestPlannerAdversarialResilience(t *testing.T) {
	for _, c := range adversarialCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			runAdversarialCase(t, c)
		})
	}
}

func runAdversarialCase(t *testing.T, c adversarialCase) {
	t.Helper()
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "planning assistant for Nomi") {
			payload, _ := json.Marshal(map[string]interface{}{
				"model": "test-model",
				"choices": []map[string]interface{}{
					{"message": map[string]interface{}{"role": "assistant", "content": c.plannerJSON}},
				},
			})
			_, _ = w.Write(payload)
			return
		}
		_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	database, err := db.New(db.Config{Path: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	providerRepo := db.NewProviderProfileRepository(database)
	if err := providerRepo.Create(&domain.ProviderProfile{
		ID: "p", Name: "F", Type: "remote", Endpoint: srv.URL,
		ModelIDs: []string{"test-model"}, Enabled: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	settings := db.NewGlobalSettingsRepository(database)
	_ = settings.SetLLMDefault("p", "test-model")

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalMgr := permissions.NewApprovalManager(db.NewApprovalRepository(database), bus)
	toolReg := tools.NewRegistry()
	_ = tools.RegisterCoreTools(toolReg)
	resolver := llm.NewResolver(providerRepo, settings, newInMemoryStore())
	_ = toolReg.Register(tools.NewLLMChatTool(resolver))
	memMgr := memory.NewEmbeddedClient(db.NewMemoryRepository(database))

	rt := runtime.NewRuntime(database, bus, permEngine, approvalMgr, tools.NewExecutor(toolReg), memMgr, runtime.DefaultConfig())
	rt.SetLLMResolver(resolver)
	defer rt.Shutdown()

	assistantRepo := db.NewAssistantRepository(database)
	_ = assistantRepo.Create(&domain.AssistantDefinition{
		ID: "a", Name: "Bot", Role: "dev",
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "llm.chat", Mode: domain.PermissionAllow},
				{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			},
		},
		Capabilities: []string{"llm.chat", "filesystem.read"},
		CreatedAt:    time.Now().UTC(),
	})

	ctx := t.Context()
	run, err := rt.CreateRun(ctx, "test", "a")
	if err != nil {
		t.Fatal(err)
	}

	// Wait up to 3 seconds for plan_review or terminal.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := db.NewRunRepository(database).GetByID(run.ID)
		if cur != nil && (cur.Status == domain.RunPlanReview || cur.Status.IsTerminal()) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	final, err := db.NewRunRepository(database).GetByID(run.ID)
	if err != nil || final == nil {
		t.Fatalf("run not found: %v", err)
	}
	plan, _ := db.NewPlanRepository(database).GetByRunID(run.ID)

	if c.wantPlanReached {
		if final.Status != domain.RunPlanReview {
			t.Fatalf("expected plan_review (recoverable malformation), got status=%s plan=%v", final.Status, plan)
		}
		if plan == nil || len(plan.Steps) == 0 {
			t.Fatalf("expected non-empty plan, got %v", plan)
		}
		return
	}

	// Hard rejection path: plan should not have persisted with the
	// hostile content. Either the run is terminal-failed OR no plan
	// row was created OR the plan row exists but has zero steps.
	if final.Status == domain.RunPlanReview && plan != nil && len(plan.Steps) > 0 {
		// Last-resort guard: if a plan WAS persisted, none of its
		// steps should reference the unknown tool — capability
		// gating + planner validation should have caught it.
		for _, s := range plan.Steps {
			if s.ExpectedTool == "web.browse" {
				t.Fatalf("hostile plan reached plan_review with unknown tool: %+v", s)
			}
		}
	}
}
