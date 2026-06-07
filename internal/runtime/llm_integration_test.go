package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/llm"
	"go.klarlabs.de/nomi/internal/memory"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// TestRunRoutesThroughLLMWhenProviderConfigured asserts the end-to-end
// plumbing that the Runtime LLM Integration feature promises:
//
//  1. A fake OpenAI-compat endpoint stands in for a real provider.
//  2. A ProviderProfile + global default points at that endpoint.
//  3. The llm.chat tool is registered.
//  4. A run is created.
//  5. planSteps picks llm.chat (because a default LLM is configured).
//  6. executeStep routes to llm.chat via StepDefinition.ExpectedTool.
//  7. The LLM responds; the step's Output contains the response.
//
// If any step in that chain breaks, this test catches it. Without this the
// four Phase-1 features could all pass unit tests and still not compose.
func TestRunRoutesThroughLLMWhenProviderConfigured(t *testing.T) {
	dir := t.TempDir()

	// Stand up a fake /chat/completions that returns a deterministic reply.
	var sawPrompt, sawSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &parsed)
		for _, m := range parsed.Messages {
			if m.Role == "user" {
				sawPrompt = m.Content
			}
			if m.Role == "system" {
				sawSystem = m.Content
			}
		}
		// Honour the streaming opt-in the runtime now sets when the
		// llm.chat tool detects a delta-callback (post #50). Mock both
		// shapes so the test exercises the real wire format.
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "data: {\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\"AI says hi\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"AI says hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))
	defer srv.Close()

	// --- wire dependencies ---
	database, err := db.New(db.Config{Path: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	secretStore := newInMemoryStore()
	_ = secretStore.Put("provider/test/api_key", "sk-fake")

	providerRepo := db.NewProviderProfileRepository(database)
	if err := providerRepo.Create(&domain.ProviderProfile{
		ID:        "test-provider",
		Name:      "Fake",
		Type:      "remote",
		Endpoint:  srv.URL,
		ModelIDs:  []string{"test-model"},
		SecretRef: "secret://provider/test/api_key",
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	settings := db.NewGlobalSettingsRepository(database)
	if err := settings.SetLLMDefault("test-provider", "test-model"); err != nil {
		t.Fatal(err)
	}

	// Event bus, permissions, tools, runtime.
	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalStore := db.NewApprovalRepository(database)
	approvalMgr := permissions.NewApprovalManager(approvalStore, bus)

	toolReg := tools.NewRegistry()
	if err := tools.RegisterCoreTools(toolReg); err != nil {
		t.Fatal(err)
	}
	resolver := llm.NewResolver(providerRepo, settings, secretStore)
	if err := toolReg.Register(tools.NewLLMChatTool(resolver)); err != nil {
		t.Fatal(err)
	}
	toolExec := tools.NewExecutor(toolReg)

	memMgr := memory.NewEmbeddedClient(db.NewMemoryRepository(database))
	rt := NewRuntime(database, bus, permEngine, approvalMgr, toolExec, memMgr, DefaultConfig())
	rt.SetLLMResolver(resolver)
	defer rt.Shutdown()

	// Seed an assistant that allows llm.chat (default BuildDefaultPolicy
	// makes this explicit in the permission engine, but the repo writes a
	// policy directly so we need to be explicit).
	assistantRepo := db.NewAssistantRepository(database)
	if err := assistantRepo.Create(&domain.AssistantDefinition{
		ID:           "a1",
		Name:         "TestBot",
		Role:         "dev",
		SystemPrompt: "You are a testing assistant. Be concise.",
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "llm.chat", Mode: domain.PermissionAllow},
			},
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Create the run. This kicks off planning + execution in a goroutine.
	run, err := rt.CreateRun(context.Background(), "say hi", "a1")
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the run to enter plan_review, then approve so execution
	// proceeds. Without this the 500ms-poll in executePlanningPhase
	// blocks forever waiting for user intent.
	approveDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(approveDeadline) {
		cur, err := db.NewRunRepository(database).GetByID(run.ID)
		if err == nil && cur.Status == domain.RunPlanReview {
			if err := rt.ApprovePlan(context.Background(), run.ID); err != nil {
				t.Fatalf("approve plan: %v", err)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Poll until the run reaches a terminal state or we time out. 6s is
	// generous for a local httptest round-trip.
	deadline := time.Now().Add(6 * time.Second)
	var finalRun *domain.Run
	var finalSteps []*domain.Step
	for time.Now().Before(deadline) {
		runs, err := db.NewRunRepository(database).List(nil, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) > 0 && runs[0].Status.IsTerminal() {
			finalRun = runs[0]
			steps, _ := db.NewStepRepository(database).ListByRun(finalRun.ID)
			finalSteps = steps
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalRun == nil {
		t.Fatal("run did not reach terminal state within deadline")
	}
	if finalRun.Status != domain.RunCompleted {
		t.Fatalf("expected completed, got %s", finalRun.Status)
	}
	if len(finalSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(finalSteps))
	}

	// The step's Output must contain the LLM reply — that's the contract
	// the whole chain exists to deliver.
	if !strings.Contains(finalSteps[0].Output, "AI says hi") {
		t.Fatalf("step output did not contain LLM reply: %q", finalSteps[0].Output)
	}

	// The LLM received the user's goal AS the prompt, and the assistant's
	// system_prompt AS the system message. If either is empty the runtime
	// dropped them on the floor.
	if sawPrompt != "say hi" {
		t.Fatalf("LLM did not receive the user goal as prompt: %q", sawPrompt)
	}
	if !strings.Contains(sawSystem, "testing assistant") {
		t.Fatalf("LLM did not receive assistant's system prompt: %q", sawSystem)
	}
}

// TestPlannerSelfRepairsOnInvalidArguments asserts that when the LLM
// emits a near-correct plan with invalid arguments (e.g. filesystem.write
// without a `content` field), the runtime feeds the validator error back
// and the second attempt produces an accepted plan. Without the repair
// loop the original feature description's "user sees Execute: <goal>
// fallback with no signal" failure mode kicks in.
func TestPlannerSelfRepairsOnInvalidArguments(t *testing.T) {
	dir := t.TempDir()

	var planCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "planning assistant for Nomi") {
			atomic.AddInt32(&planCalls, 1)
			// First call: invalid arguments (filesystem.write without content).
			// Second call (after repair hint): well-formed plan with both fields.
			if atomic.LoadInt32(&planCalls) == 1 {
				_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"{\"steps\":[{\"title\":\"Write file\",\"description\":\"Save output\",\"tool\":\"filesystem.write\",\"arguments\":{\"path\":\"out.txt\"}}]}"}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"{\"steps\":[{\"title\":\"Write file\",\"description\":\"Save output\",\"tool\":\"filesystem.write\",\"arguments\":{\"path\":\"out.txt\",\"content\":\"hello\"}}]}"}}]}`)
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

	secretStore := newInMemoryStore()
	providerRepo := db.NewProviderProfileRepository(database)
	_ = providerRepo.Create(&domain.ProviderProfile{
		ID: "p", Name: "F", Type: "remote", Endpoint: srv.URL,
		ModelIDs: []string{"test-model"}, Enabled: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	settings := db.NewGlobalSettingsRepository(database)
	_ = settings.SetLLMDefault("p", "test-model")

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalMgr := permissions.NewApprovalManager(db.NewApprovalRepository(database), bus)
	toolReg := tools.NewRegistry()
	_ = tools.RegisterCoreTools(toolReg)
	resolver := llm.NewResolver(providerRepo, settings, secretStore)
	_ = toolReg.Register(tools.NewLLMChatTool(resolver))
	memMgr := memory.NewEmbeddedClient(db.NewMemoryRepository(database))

	rt := NewRuntime(database, bus, permEngine, approvalMgr, tools.NewExecutor(toolReg), memMgr, DefaultConfig())
	rt.SetLLMResolver(resolver)
	defer rt.Shutdown()

	assistantRepo := db.NewAssistantRepository(database)
	_ = assistantRepo.Create(&domain.AssistantDefinition{
		ID: "a", Name: "Bot", Role: "dev",
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "filesystem.write", Mode: domain.PermissionAllow},
				{Capability: "llm.chat", Mode: domain.PermissionAllow},
			},
		},
		CreatedAt: time.Now().UTC(),
	})

	run, err := rt.CreateRun(context.Background(), "save output", "a")
	if err != nil {
		t.Fatal(err)
	}

	// Wait for plan_review (would be RunFailed if both planner attempts
	// were rejected and no fallback applied).
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		cur, _ := db.NewRunRepository(database).GetByID(run.ID)
		if cur != nil && (cur.Status == domain.RunPlanReview || cur.Status == domain.RunExecuting || cur.Status.IsTerminal()) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Two planner calls: original + one repair turn. If repair didn't
	// fire we'd see only one.
	if got := atomic.LoadInt32(&planCalls); got != 2 {
		t.Fatalf("expected exactly 2 planner calls (initial + repair), got %d", got)
	}

	// Step in the persisted plan must use filesystem.write — proves the
	// repaired plan landed, not the legacy llm.chat fallback.
	planRepo := db.NewPlanRepository(database)
	plan, err := planRepo.GetByRunID(run.ID)
	if err != nil || plan == nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].ExpectedTool != "filesystem.write" {
		t.Fatalf("expected one filesystem.write step, got %+v", plan.Steps)
	}
}

// TestMultiStepPlannerProducesMultipleSteps asserts feature #33:
// when the LLM returns a valid JSON plan with multiple steps, the runtime
// persists each as a distinct StepDefinition + executable Step, and all
// of them run through executeStep to completion.
func TestMultiStepPlannerProducesMultipleSteps(t *testing.T) {
	dir := t.TempDir()

	// The fake endpoint returns a multi-step plan on the planner call,
	// then a normal chat response for each step. We distinguish by the
	// planner's system prompt showing up in the request body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "planning assistant for Nomi") {
			// Return a 2-step plan.
			_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"{\"steps\":[{\"title\":\"Think\",\"description\":\"Consider the question\",\"tool\":\"llm.chat\"},{\"title\":\"Respond\",\"description\":\"Produce the answer\",\"tool\":\"llm.chat\"}]}"}}]}`)
			return
		}
		// Normal chat reply (for each of the two steps).
		_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"role":"assistant","content":"step done"}}]}`)
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

	secretStore := newInMemoryStore()
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
	resolver := llm.NewResolver(providerRepo, settings, secretStore)
	_ = toolReg.Register(tools.NewLLMChatTool(resolver))
	memMgr := memory.NewEmbeddedClient(db.NewMemoryRepository(database))

	rt := NewRuntime(database, bus, permEngine, approvalMgr, tools.NewExecutor(toolReg), memMgr, DefaultConfig())
	rt.SetLLMResolver(resolver)
	defer rt.Shutdown()

	assistantRepo := db.NewAssistantRepository(database)
	_ = assistantRepo.Create(&domain.AssistantDefinition{
		ID: "a", Name: "Bot", Role: "dev", SystemPrompt: "be terse",
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "llm.chat", Mode: domain.PermissionAllow},
			},
		},
		CreatedAt: time.Now().UTC(),
	})

	run, err := rt.CreateRun(context.Background(), "answer a question", "a")
	if err != nil {
		t.Fatal(err)
	}

	// Wait for plan_review, approve.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		cur, _ := db.NewRunRepository(database).GetByID(run.ID)
		if cur != nil && cur.Status == domain.RunPlanReview {
			if err := rt.ApprovePlan(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Poll to completion.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		cur, _ := db.NewRunRepository(database).GetByID(run.ID)
		if cur != nil && cur.Status.IsTerminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	final, _ := db.NewRunRepository(database).GetByID(run.ID)
	if final == nil || final.Status != domain.RunCompleted {
		t.Fatalf("want completed run, got %v", final)
	}

	steps, err := db.NewStepRepository(database).ListByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps from planner; got %d", len(steps))
	}
	for _, s := range steps {
		if s.Status != domain.StepDone {
			t.Fatalf("step %q status = %s (output=%q)", s.Title, s.Status, s.Output)
		}
	}
}

// newInMemoryStore is a local copy of the test secret store used across
// llm + tools tests. Lives here so this test package doesn't depend on
// those packages' test internals.
func newInMemoryStore() *inMemoryStore {
	return &inMemoryStore{data: make(map[string]string)}
}

type inMemoryStore struct {
	data map[string]string
}

func (s *inMemoryStore) Put(k, v string) error { s.data[k] = v; return nil }
func (s *inMemoryStore) Get(k string) (string, error) {
	v, ok := s.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (s *inMemoryStore) Delete(k string) error { delete(s.data, k); return nil }
