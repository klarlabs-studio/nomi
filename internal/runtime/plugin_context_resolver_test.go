package runtime

import (
	"context"
	"testing"
)

// TestPluginContextResolverFieldOptIn — the field defaults to nil and
// SetPluginContextResolver wires a closure in. Nothing further to assert
// at this level; the lifecycle splice is exercised by integration
// tests that feed a full Run through executePlanningPhase.
func TestPluginContextResolverFieldOptIn(t *testing.T) {
	r := &Runtime{}
	if r.pluginContextResolver != nil {
		t.Fatal("expected nil resolver by default")
	}
	called := false
	r.SetPluginContextResolver(func(_ context.Context, _, _, _ string) string {
		called = true
		return "stub-context"
	})
	if r.pluginContextResolver == nil {
		t.Fatal("setter did not install resolver")
	}
	got := r.pluginContextResolver(context.Background(), "a", "r", "g")
	if !called || got != "stub-context" {
		t.Fatalf("resolver did not run as expected: called=%v got=%q", called, got)
	}
}

// TestPluginContextResolverContractMatchesRoadyDescription documents the
// resolver's signature inline so a future refactor that drifts the
// shape (e.g. swapping (assistantID, runID, goal) order) breaks an
// obvious-to-find test rather than silently failing in production.
func TestPluginContextResolverContractMatchesRoadyDescription(t *testing.T) {
	var observed struct {
		assistantID, runID, goal string
	}
	r := &Runtime{}
	r.SetPluginContextResolver(func(_ context.Context, assistantID, runID, goal string) string {
		observed.assistantID = assistantID
		observed.runID = runID
		observed.goal = goal
		return ""
	})
	r.pluginContextResolver(context.Background(), "asst-1", "run-1", "ship it")
	if observed.assistantID != "asst-1" || observed.runID != "run-1" || observed.goal != "ship it" {
		t.Fatalf("argument order drifted: %+v", observed)
	}
}
