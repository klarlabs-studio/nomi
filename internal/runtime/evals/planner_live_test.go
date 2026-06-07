package evals

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/llm"
	"go.klarlabs.de/nomi/internal/memory"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// liveProvider describes how to point the planner at a real LLM
// during the live eval matrix. Each one is opt-in via environment
// variables — absent envs cause the provider to skip silently so a
// developer can run a single provider without configuring the others.
type liveProvider struct {
	name     string // "ollama", "openai", "anthropic"
	endpoint string
	apiKey   string
	model    string
	// thresholdEnv overrides the per-provider pass-rate floor.
	// Pattern: NOMI_GOLDEN_THRESHOLD_OLLAMA, NOMI_GOLDEN_THRESHOLD_OPENAI, etc.
	thresholdEnv string
}

// liveProviders collects every provider whose envs are populated.
// Returns empty when nothing is configured — TestPlannerGoldenSet_Live
// skips entirely in that case.
func liveProviders() []liveProvider {
	out := []liveProvider{}

	// Ollama (local). Endpoint defaults to the convention used by
	// Ollama's OpenAI-compat shim; override with NOMI_EVAL_LIVE_OLLAMA_URL.
	if model := os.Getenv("NOMI_EVAL_LIVE_OLLAMA_MODEL"); model != "" {
		endpoint := os.Getenv("NOMI_EVAL_LIVE_OLLAMA_URL")
		if endpoint == "" {
			endpoint = "http://127.0.0.1:11434/v1"
		}
		out = append(out, liveProvider{
			name:         "ollama",
			endpoint:     endpoint,
			apiKey:       "",
			model:        model,
			thresholdEnv: "NOMI_GOLDEN_THRESHOLD_OLLAMA",
		})
	}

	// OpenAI. Endpoint is fixed by the SaaS provider; model varies.
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		if model := os.Getenv("NOMI_EVAL_LIVE_OPENAI_MODEL"); model != "" {
			out = append(out, liveProvider{
				name:         "openai",
				endpoint:     "https://api.openai.com/v1",
				apiKey:       key,
				model:        model,
				thresholdEnv: "NOMI_GOLDEN_THRESHOLD_OPENAI",
			})
		}
	}

	// Anthropic. Endpoint is the native Messages API.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		if model := os.Getenv("NOMI_EVAL_LIVE_ANTHROPIC_MODEL"); model != "" {
			out = append(out, liveProvider{
				name:         "anthropic",
				endpoint:     "https://api.anthropic.com/v1",
				apiKey:       key,
				model:        model,
				thresholdEnv: "NOMI_GOLDEN_THRESHOLD_ANTHROPIC",
			})
		}
	}

	return out
}

// TestPlannerGoldenSet_Live runs the goldenCases against each
// configured live provider and reports a per-provider pass rate. The
// test is skipped unless at least one provider's envs are present.
//
// Run-path examples (set the envs you want to exercise):
//
//	NOMI_EVAL_LIVE_OLLAMA_MODEL=qwen2.5:14b \
//	OPENAI_API_KEY=... NOMI_EVAL_LIVE_OPENAI_MODEL=gpt-4o-mini \
//	make eval-live-providers
//
// Thresholds: NOMI_GOLDEN_THRESHOLD_<PROVIDER> overrides the global
// NOMI_GOLDEN_THRESHOLD (default 0.80) on a per-provider basis. A
// per-provider failure marks the suite red so a regression doesn't
// hide behind a stronger provider's average.
func TestPlannerGoldenSet_Live(t *testing.T) {
	providers := liveProviders()
	if len(providers) == 0 {
		t.Skip("no live provider envs configured; set NOMI_EVAL_LIVE_OLLAMA_MODEL / OPENAI_API_KEY+model / ANTHROPIC_API_KEY+model to opt in")
	}

	global := loadThreshold(t, 0.80)
	failed := []string{}

	for _, p := range providers {
		threshold := global
		if v := os.Getenv(p.thresholdEnv); v != "" {
			if parsed, err := parseFloat01(v); err == nil {
				threshold = parsed
			}
		}

		// Use a captured *testing.T per-case so an individual
		// fixture failure doesn't mark the parent test red — the
		// aggregate threshold below is the real pass/fail gate.
		// A "real" LLM legitimately produces one-off plan shapes
		// that don't match a fixture; what we care about is the
		// percentage that do.
		pass := 0
		for _, c := range goldenCases {
			if runGoldenCaseLiveSafe(t, p, c) {
				pass++
			}
		}
		rate := float64(pass) / float64(len(goldenCases))
		t.Logf("planner golden pass rate [provider=%s model=%s]: %d/%d = %.1f%% (threshold %.2f)",
			p.name, p.model, pass, len(goldenCases), rate*100, threshold)
		if rate < threshold {
			failed = append(failed, fmt.Sprintf("%s: %.2f < %.2f", p.name, rate, threshold))
		}
	}

	if len(failed) > 0 {
		t.Fatalf("live provider pass rate below threshold: %s", strings.Join(failed, "; "))
	}
}

func parseFloat01(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0, err
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("out of range")
	}
	return f, nil
}

// runGoldenCaseAgainstProvider drives one golden fixture's goal
// through a real LLM. Same structure as runGoldenCase but skips the
// httptest fake — the planner's output here comes from the actual
// model, so the test asserts on the resulting step count + tool
// routing rather than expecting a byte-identical response.
//
// Tolerance: the planner can legitimately produce more steps than the
// fixture's exact count (e.g. an LLM that includes a "review" step
// before the tool call). We allow within ±1 step and require the
// FIRST step's tool to match c.wantTools[0]. This keeps the test
// useful as a regression signal without being a fragile string match.
// runGoldenCaseLiveSafe wraps the live-provider runner and returns
// (passed, error message). It logs the per-case outcome through the
// parent *testing.T without calling t.Fatal/t.Error, so individual
// non-matches don't make the parent suite red — the aggregate
// threshold check in TestPlannerGoldenSet_Live owns pass/fail.
func runGoldenCaseLiveSafe(parent *testing.T, p liveProvider, c goldenCase) bool {
	got, want, err := runLiveCase(p, c)
	if err != nil {
		parent.Logf("FAIL %s/%s: %v", p.name, c.name, err)
		return false
	}
	if got != want {
		parent.Logf("FAIL %s/%s: %s", p.name, c.name, got)
		return false
	}
	parent.Logf("PASS %s/%s", p.name, c.name)
	return true
}

// runLiveCase drives one fixture through a real provider. Returns:
//   - got: human-readable description of what the planner produced
//   - want: expected description (only meaningful when got != want)
//   - err: infrastructure failure (DB, network, runtime) distinct from
//     a fixture mismatch — caller treats both as "did not pass" but
//     can surface infra errors differently.
//
// Doesn't use *testing.T because the caller (runGoldenCaseLiveSafe)
// owns the aggregate pass/fail decision; t.Fatal/t.Error inside would
// turn a single fixture mismatch into a hard failure.
func runLiveCase(p liveProvider, c goldenCase) (got, want string, err error) {
	dir, err := os.MkdirTemp("", "nomi-eval-live-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)

	database, err := db.New(db.Config{Path: dir + "/test.db"})
	if err != nil {
		return "", "", err
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		return "", "", err
	}

	providerRepo := db.NewProviderProfileRepository(database)
	secretStore := newInMemoryStore()
	secretRef := ""
	if p.apiKey != "" {
		const key = "live-eval/api-key"
		_ = secretStore.Put(key, p.apiKey)
		secretRef = "secret://" + key
	}
	if err := providerRepo.Create(&domain.ProviderProfile{
		ID: "live", Name: p.name, Type: "remote",
		Endpoint: p.endpoint, ModelIDs: []string{p.model},
		SecretRef: secretRef, Enabled: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return "", "", err
	}
	settings := db.NewGlobalSettingsRepository(database)
	_ = settings.SetLLMDefault("live", p.model)

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalMgr := permissions.NewApprovalManager(db.NewApprovalRepository(database), bus)
	toolReg := tools.NewRegistry()
	_ = tools.RegisterCoreTools(toolReg)
	resolver := llm.NewResolver(providerRepo, settings, secretStore)
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
		return "", "", err
	}

	// Real LLM calls take seconds — give the planner room.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		cur, _ := db.NewRunRepository(database).GetByID(run.ID)
		if cur != nil && (cur.Status == domain.RunPlanReview || cur.Status.IsTerminal()) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	plan, planErr := db.NewPlanRepository(database).GetByRunID(run.ID)
	if planErr != nil || plan == nil {
		return "no-plan", fmt.Sprintf("steps=%d tool=%s", c.wantSteps, c.wantTools[0]),
			fmt.Errorf("no plan persisted: %v", planErr)
	}
	if len(plan.Steps) == 0 {
		return "empty-plan", "non-empty plan", nil
	}
	// Tolerance: real models often add an extra wrap-up or review
	// step. Allow within ±1 of the fixture's wantSteps so the
	// regression signal stays useful without becoming brittle.
	gotSteps := len(plan.Steps)
	if abs(gotSteps-c.wantSteps) > 1 {
		return fmt.Sprintf("steps=%d", gotSteps),
			fmt.Sprintf("steps=%d (±1)", c.wantSteps), nil
	}
	if plan.Steps[0].ExpectedTool != c.wantTools[0] {
		return fmt.Sprintf("first-tool=%s", plan.Steps[0].ExpectedTool),
			fmt.Sprintf("first-tool=%s", c.wantTools[0]), nil
	}
	return "ok", "ok", nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
