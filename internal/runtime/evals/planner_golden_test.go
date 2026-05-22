package evals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/llm"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/runtime"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// goldenCase pairs a user goal with the plan a competent planner should
// produce. The httptest fake replays plannerJSON when it sees the
// planner's system prompt; everything else returns "ok" so the runtime
// can complete the run end-to-end. Assertions run against the persisted
// plan_steps after plan_review fires.
type goldenCase struct {
	name        string
	goal        string
	plannerJSON string
	wantTools   []string // ordered list of expected_tool per step
	wantSteps   int      // exact step count
}

// goldenCases is the eval corpus. Each fixture covers one realistic
// user intent with a different expected tool routing pattern.
//
// Run with:   go test ./internal/runtime/evals/ -run TestPlannerGoldenSet
// Set NOMI_GOLDEN_THRESHOLD to override the 80%-pass threshold.
var goldenCases = []goldenCase{
	{
		name: "single-step llm.chat for a question",
		goal: "Explain SQLite WAL mode in 4 sentences.",
		plannerJSON: `{"steps":[{"title":"Explain WAL","description":"Summarize WAL.","tool":"llm.chat","arguments":{"prompt":"Explain SQLite WAL in 4 sentences."}}]}`,
		wantTools: []string{"llm.chat"},
		wantSteps: 1,
	},
	{
		name: "read-then-summarize is two steps",
		goal: "Summarize notes.md.",
		plannerJSON: `{"steps":[
			{"title":"Read notes","description":"Pull notes.md.","tool":"filesystem.read","arguments":{"path":"notes.md"}},
			{"title":"Summarize notes","description":"Five-bullet summary.","tool":"llm.chat","arguments":{"prompt":"Summarize the file you just read into 5 bullets."}}
		]}`,
		wantTools: []string{"filesystem.read", "llm.chat"},
		wantSteps: 2,
	},
	{
		name: "write needs a content arg",
		goal: "Save a placeholder TODO.md file.",
		plannerJSON: `{"steps":[{"title":"Write TODO","description":"Create TODO.md.","tool":"filesystem.write","arguments":{"path":"TODO.md","content":"# TODO\n\n- [ ] Pick first task\n"}}]}`,
		wantTools: []string{"filesystem.write"},
		wantSteps: 1,
	},
	{
		name: "list+read combo for orientation",
		goal: "Find the README in this folder and quote its title.",
		plannerJSON: `{"steps":[
			{"title":"List folder","description":"Inspect what's here.","tool":"filesystem.list","arguments":{}},
			{"title":"Read README","description":"Pull README contents.","tool":"filesystem.read","arguments":{"path":"README.md"}},
			{"title":"Quote title","description":"Extract the H1.","tool":"llm.chat","arguments":{"prompt":"Quote the H1 from the file you just read."}}
		]}`,
		wantTools: []string{"filesystem.list", "filesystem.read", "llm.chat"},
		wantSteps: 3,
	},
	{
		name: "command.exec for running tests",
		goal: "Run go test ./...",
		plannerJSON: `{"steps":[{"title":"Run tests","description":"Execute go test.","tool":"command.exec","arguments":{"command":"go test ./..."}}]}`,
		wantTools: []string{"command.exec"},
		wantSteps: 1,
	},
}

// TestPlannerGoldenSet runs every fixture against an in-memory runtime
// + httptest fake LLM. Reports per-case pass/fail and a final pass
// rate. Goes red below the threshold so a planner regression in
// validation, parsing, or routing surfaces here.
//
// NOMI_GOLDEN_THRESHOLD overrides the default 0.80 — useful in CI for
// pinning a stricter floor on the fake-LLM corpus where there's no
// real model variance.
func TestPlannerGoldenSet(t *testing.T) {
	threshold := loadThreshold(t, 0.80)

	pass := 0
	for _, c := range goldenCases {
		c := c
		ok := t.Run(c.name, func(t *testing.T) {
			runGoldenCase(t, c)
		})
		if ok {
			pass++
		}
	}
	rate := float64(pass) / float64(len(goldenCases))
	provider := "fake-llm"
	t.Logf("planner golden pass rate [provider=%s]: %d/%d = %.1f%%", provider, pass, len(goldenCases), rate*100)
	if rate < threshold {
		t.Fatalf("planner golden pass rate [provider=%s] %.2f below threshold %.2f", provider, rate, threshold)
	}
}

// loadThreshold reads NOMI_GOLDEN_THRESHOLD or falls back to the
// supplied default. Was previously dead code (just logged the env
// value); now it's the actual gate so an env override can tighten the
// floor in CI without code changes.
func loadThreshold(t *testing.T, def float64) float64 {
	t.Helper()
	v := os.Getenv("NOMI_GOLDEN_THRESHOLD")
	if v == "" {
		return def
	}
	parsed, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Logf("NOMI_GOLDEN_THRESHOLD=%q is not a float; using default %.2f", v, def)
		return def
	}
	if parsed < 0 || parsed > 1 {
		t.Logf("NOMI_GOLDEN_THRESHOLD=%v out of range [0,1]; using default %.2f", parsed, def)
		return def
	}
	return parsed
}

func runGoldenCase(t *testing.T, c goldenCase) {
	t.Helper()
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "planning assistant for Nomi") {
			// Wrap the fixture JSON in an OpenAI-shaped chat response.
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
	memMgr := memory.NewTestClient(t)

	rt := runtime.NewRuntime(database, bus, permEngine, approvalMgr, tools.NewExecutor(toolReg), memMgr, runtime.DefaultConfig())
	rt.SetLLMResolver(resolver)
	defer rt.Shutdown()

	assistantRepo := db.NewAssistantRepository(database)
	_ = assistantRepo.Create(&domain.AssistantDefinition{
		ID: "a", Name: "Bot", Role: "dev",
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				// Permit every tool the corpus exercises so plan_review
				// is the gate, not capability filtering.
				{Capability: "llm.chat", Mode: domain.PermissionAllow},
				{Capability: "filesystem.read", Mode: domain.PermissionAllow},
				{Capability: "filesystem.write", Mode: domain.PermissionAllow},
				{Capability: "filesystem.list", Mode: domain.PermissionAllow},
				{Capability: "filesystem.context", Mode: domain.PermissionAllow},
				{Capability: "command.exec", Mode: domain.PermissionAllow},
			},
		},
		Capabilities: []string{
			"llm.chat", "filesystem.read", "filesystem.write",
			"filesystem.list", "filesystem.context", "command.exec",
		},
		CreatedAt: time.Now().UTC(),
	})

	run, err := rt.CreateRun(context.Background(), c.goal, "a")
	if err != nil {
		t.Fatal(err)
	}

	// Wait for plan_review or terminal.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		cur, _ := db.NewRunRepository(database).GetByID(run.ID)
		if cur == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if cur.Status == domain.RunPlanReview || cur.Status.IsTerminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	plan, err := db.NewPlanRepository(database).GetByRunID(run.ID)
	if err != nil || plan == nil {
		t.Fatalf("no plan persisted: %v", err)
	}
	if len(plan.Steps) != c.wantSteps {
		t.Fatalf("step count = %d, want %d (%+v)", len(plan.Steps), c.wantSteps, plan.Steps)
	}
	for i, want := range c.wantTools {
		if plan.Steps[i].ExpectedTool != want {
			t.Fatalf("step %d tool = %q, want %q", i, plan.Steps[i].ExpectedTool, want)
		}
	}
}

// inMemoryStore is a local secrets.Store fake. The eval driver doesn't
// actually need any credentials (httptest server has no auth), but
// llm.NewResolver requires a non-nil Store, so a no-op fake is enough.
type inMemoryStore struct{ data map[string]string }

func newInMemoryStore() *inMemoryStore { return &inMemoryStore{data: make(map[string]string)} }
func (s *inMemoryStore) Put(k, v string) error { s.data[k] = v; return nil }
func (s *inMemoryStore) Get(k string) (string, error) {
	v, ok := s.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (s *inMemoryStore) Delete(k string) error { delete(s.data, k); return nil }
