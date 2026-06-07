package permissions

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestEvaluateForConnection_OverrideTakesPrecedence(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "gmail.send", Mode: domain.PermissionAllow},
		},
		PerConnectionOverrides: []domain.PerConnectionOverride{
			{ConnectionID: "work", Capability: "gmail.send", Mode: domain.PermissionConfirm},
		},
	}
	// Work connection should require confirm.
	if got := e.EvaluateForConnection(policy, "gmail.send", "work"); got != domain.PermissionConfirm {
		t.Fatalf("work override should win; got %s", got)
	}
	// Personal connection has no override — falls through to allow.
	if got := e.EvaluateForConnection(policy, "gmail.send", "personal"); got != domain.PermissionAllow {
		t.Fatalf("personal should fall through to allow; got %s", got)
	}
	// Empty connection id behaves like Evaluate.
	if got := e.EvaluateForConnection(policy, "gmail.send", ""); got != domain.PermissionAllow {
		t.Fatalf("empty connection id should be allow; got %s", got)
	}
}

func TestEvaluateForConnection_OverrideOnUnmatchedCapabilityDoesNothing(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "gmail.send", Mode: domain.PermissionAllow},
		},
		PerConnectionOverrides: []domain.PerConnectionOverride{
			// Override targets a different capability — should not leak.
			{ConnectionID: "work", Capability: "gmail.archive", Mode: domain.PermissionDeny},
		},
	}
	if got := e.EvaluateForConnection(policy, "gmail.send", "work"); got != domain.PermissionAllow {
		t.Fatalf("unrelated override should not affect gmail.send; got %s", got)
	}
}

func TestMatchingOverrideOrRule_PreservesConstraints(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{
		PerConnectionOverrides: []domain.PerConnectionOverride{
			{
				ConnectionID: "work",
				Capability:   "command.exec",
				Mode:         domain.PermissionConfirm,
				Constraints:  map[string]interface{}{"allowed_binaries": []string{"git", "make"}},
			},
		},
	}
	got := e.MatchingOverrideOrRule(policy, "command.exec", "work")
	if got == nil {
		t.Fatal("expected rule from override")
	}
	if got.Mode != domain.PermissionConfirm {
		t.Fatalf("mode: %s", got.Mode)
	}
	bins, _ := got.Constraints["allowed_binaries"].([]string)
	if len(bins) != 2 || bins[0] != "git" {
		t.Fatalf("constraints lost: %+v", got.Constraints)
	}
}

func TestFindOverride_ExactMatchOnly(t *testing.T) {
	policy := domain.PermissionPolicy{
		PerConnectionOverrides: []domain.PerConnectionOverride{
			{ConnectionID: "a", Capability: "gmail.send", Mode: domain.PermissionAllow},
		},
	}
	if ov := findOverride(policy, "a", "gmail.send"); ov == nil {
		t.Fatal("exact match should find override")
	}
	// Wildcard capability is NOT expanded for overrides — they must match
	// the invocation's exact capability string.
	if ov := findOverride(policy, "a", "gmail.*"); ov != nil {
		t.Fatal("findOverride should not treat gmail.* as a pattern")
	}
}
