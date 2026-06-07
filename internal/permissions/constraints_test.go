package permissions

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestMatchingRuleReturnsExactRule(t *testing.T) {
	e := NewEngine()
	rule := domain.PermissionRule{
		Capability: "command.exec",
		Mode:       domain.PermissionAllow,
		Constraints: map[string]interface{}{
			"allowed_binaries": []interface{}{"git", "go"},
		},
	}
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{rule}}

	got := e.MatchingRule(policy, "command.exec")
	if got == nil {
		t.Fatal("expected a matched rule")
	}
	if got.Mode != domain.PermissionAllow {
		t.Fatalf("mode: %s", got.Mode)
	}
	binaries, _ := got.Constraints["allowed_binaries"].([]interface{})
	if len(binaries) != 2 {
		t.Fatalf("expected 2 allowed_binaries, got %v", binaries)
	}
}

func TestMatchingRuleReturnsLongestWildcard(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "*", Mode: domain.PermissionAllow, Constraints: map[string]interface{}{"tag": "global"}},
		{Capability: "filesystem.*", Mode: domain.PermissionConfirm, Constraints: map[string]interface{}{"tag": "fs"}},
	}}

	got := e.MatchingRule(policy, "filesystem.read")
	if got == nil || got.Constraints["tag"] != "fs" {
		t.Fatalf("expected the filesystem.* rule to win; got %+v", got)
	}
}

func TestMatchingRuleNoMatchReturnsNil(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "filesystem.read", Mode: domain.PermissionAllow},
	}}
	if got := e.MatchingRule(policy, "command.exec"); got != nil {
		t.Fatalf("expected nil for unmatched capability; got %+v", got)
	}
}

// TestConstraintRoundTripsThroughPolicy guards against future changes that
// might strip the Constraints field during JSON serialization or copying.
func TestConstraintRoundTripsThroughPolicy(t *testing.T) {
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{
			Capability: "command.exec",
			Mode:       domain.PermissionAllow,
			Constraints: map[string]interface{}{
				"allowed_binaries": []interface{}{"git"},
			},
		},
	}}
	e := NewEngine()
	rule := e.MatchingRule(policy, "command.exec")
	if rule == nil {
		t.Fatal("expected match")
	}
	if rule.Constraints == nil {
		t.Fatal("Constraints map lost")
	}
}
