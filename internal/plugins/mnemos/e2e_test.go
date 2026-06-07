//go:build e2e_mnemos

// E2E test suite for the Mnemos plugin. Gated behind the `e2e_mnemos`
// build tag so the default test sweep stays fast — the suite expects
// a real `mnemos serve` reachable at MNEMOS_E2E_BASE_URL.
//
// Run locally:
//
//	go run go.klarlabs.de/mnemos/cmd/mnemos serve --port 9099 &
//	MNEMOS_E2E_BASE_URL=http://127.0.0.1:9099 \
//	  go test -tags e2e_mnemos -v ./internal/plugins/mnemos/...
//
// Run in CI: .github/workflows/e2e-mnemos.yml handles the orchestration.

package mnemos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/plugins"
)

// e2eBaseURL returns the configured Mnemos URL or skips the test if
// the environment is missing. Avoids accidental "passes" when CI
// forgot to set MNEMOS_E2E_BASE_URL.
func e2eBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MNEMOS_E2E_BASE_URL")
	if url == "" {
		t.Skip("MNEMOS_E2E_BASE_URL not set — skipping e2e suite")
	}
	return url
}

// setupE2EPlugin configures a Mnemos plugin pointing at the running
// `mnemos serve` instance. Returns the plugin ready to invoke tools
// against connection id "e2e".
//
// MNEMOS_E2E_TOKEN, when set, is plumbed through fakeSecrets so the
// real Mnemos server can authenticate the test's writes; absent, the
// plugin runs token-less (works against a dev server with auth
// disabled).
func setupE2EPlugin(t *testing.T) *Plugin {
	t.Helper()
	secretStore := &fakeSecrets{}
	tokenRef := ""
	if tok := os.Getenv("MNEMOS_E2E_TOKEN"); tok != "" {
		_ = secretStore.Put("e2e-token", tok)
		tokenRef = "e2e-token"
	}
	p := New(secretStore)
	cfg, err := json.Marshal(configureInput{
		Connections: []connectionConfig{
			{
				ID:                "e2e",
				BaseURL:           e2eBaseURL(t),
				VisibilityDefault: "team",
				TokenRef:          tokenRef,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

// findTool returns the tool with the given name, failing the test if
// the plugin didn't register one. Reaches through Tools() so the
// indirection layer is exercised end-to-end.
func findTool(t *testing.T, p *Plugin, name string) toolExecutor {
	t.Helper()
	for _, tl := range p.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

// toolExecutor is the subset of the tools.Tool interface this test
// suite cares about. Local alias avoids a cross-package import.
type toolExecutor interface {
	Name() string
	Capability() string
	Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error)
}

// TestE2E_FullRoundTrip exercises every tool the plugin exposes
// against a real Mnemos server. Assertions stay loose (counts, type
// shapes) because the upstream server fills in IDs / timestamps that
// can't be predicted.
func TestE2E_FullRoundTrip(t *testing.T) {
	p := setupE2EPlugin(t)
	ctx := context.Background()
	runID := fmt.Sprintf("nomi-e2e-%d", time.Now().UnixNano())

	// 1. Append 3 events. Mnemos v0.15.3 requires id + content +
	// source_input_id + timestamp on every event; derive ids from the
	// run id so reruns inside the same process don't collide on the
	// server's uniqueness check.
	now := time.Now().UTC().Format(time.RFC3339)
	eventsAppend := findTool(t, p, ToolEventsAppend)
	out, err := eventsAppend.Execute(ctx, map[string]interface{}{
		"connection_id": "e2e",
		"events": []interface{}{
			map[string]interface{}{"id": runID + "-evt-1", "run_id": runID, "source_input_id": runID, "timestamp": now, "content": "observed: API latency spike at 14:02"},
			map[string]interface{}{"id": runID + "-evt-2", "run_id": runID, "source_input_id": runID, "timestamp": now, "content": "observed: error rate normal across all regions"},
			map[string]interface{}{"id": runID + "-evt-3", "run_id": runID, "source_input_id": runID, "timestamp": now, "content": "decision rationale: rollback canary"},
		},
	})
	if err != nil {
		t.Fatalf("events.append: %v", err)
	}
	if acc, _ := out["accepted"].(int); acc != 3 {
		t.Errorf("events.append accepted = %v, want 3", out["accepted"])
	}

	// 2. Append 2 claims (no evidence — keeps the assertion bounded).
	// Claims also require an explicit id per Mnemos v0.15.3 validation.
	claimsAppend := findTool(t, p, ToolClaimsAppend)
	out, err = claimsAppend.Execute(ctx, map[string]interface{}{
		"connection_id": "e2e",
		"claims": []interface{}{
			map[string]interface{}{
				"id":         runID + "-claim-1",
				"text":       "Canary rollback was the correct call",
				"type":       "decision",
				"confidence": 0.85,
				"status":     "active",
			},
			map[string]interface{}{
				"id":         runID + "-claim-2",
				"text":       "Latency spike traces to upstream provider",
				"type":       "hypothesis",
				"confidence": 0.6,
				"status":     "active",
			},
		},
	})
	if err != nil {
		t.Fatalf("claims.append: %v", err)
	}
	if acc, _ := out["accepted"].(int); acc != 2 {
		t.Errorf("claims.append accepted = %v, want 2", out["accepted"])
	}

	// 3. List claims by type.
	claimsList := findTool(t, p, ToolClaimsList)
	out, err = claimsList.Execute(ctx, map[string]interface{}{
		"connection_id": "e2e",
		"type":          "decision",
		"limit":         10,
	})
	if err != nil {
		t.Fatalf("claims.list: %v", err)
	}
	claims, ok := out["claims"]
	if !ok || claims == nil {
		t.Errorf("claims.list missing 'claims' field: %+v", out)
	}

	// 4. List relationships (expected empty — we never appended any).
	if _, err := findTool(t, p, ToolRelationshipsList).Execute(ctx, map[string]interface{}{
		"connection_id": "e2e",
		"limit":         10,
	}); err != nil {
		t.Fatalf("relationships.list: %v", err)
	}

	// 5. Append a single 4-dim embedding (any non-zero size works for
	// the wire-format assertion; real models ship 1536-3072 dims).
	embAppend := findTool(t, p, ToolEmbeddingsAppend)
	vec := []interface{}{0.1, 0.2, 0.3, 0.4}
	out, err = embAppend.Execute(ctx, map[string]interface{}{
		"connection_id": "e2e",
		"embeddings": []interface{}{
			map[string]interface{}{
				"entity_id":   runID,
				"entity_type": "event",
				"vector":      vec,
				"model":       "e2e-stub",
				"dimensions":  4,
			},
		},
	})
	if err != nil {
		t.Fatalf("embeddings.append: %v", err)
	}
	if acc, _ := out["accepted"].(int); acc != 1 {
		t.Errorf("embeddings.append accepted = %v, want 1", out["accepted"])
	}

	// 6. Hybrid search.
	out, err = findTool(t, p, ToolSearch).Execute(ctx, map[string]interface{}{
		"connection_id": "e2e",
		"query":         "rollback",
		"top_k":         5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, ok := out["claims"]; !ok {
		t.Errorf("search response missing 'claims': %+v", out)
	}

	// 7. Context source: requests the rendered context block for
	// runID. Server should at least return a non-error response with
	// the run_id echoed in the body shape — content may be empty if
	// claim extraction is async.
	sources := p.ContextSources()
	if len(sources) != 1 {
		t.Fatalf("want 1 context source, got %d", len(sources))
	}
	if _, err := sources[0].Query(ctx, plugins.ContextQueryRequest{
		RunID:     runID,
		Goal:      "rollback",
		MaxTokens: 1000,
	}); err != nil {
		t.Errorf("context source Query: %v", err)
	}
}
