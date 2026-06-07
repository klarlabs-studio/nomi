package permissions

import (
	"errors"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestValidatePolicyAgainstCeiling_AcceptsCoherentPolicy(t *testing.T) {
	caps := []string{"filesystem", "command", "web"}
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
			{Capability: "network.outgoing", Mode: domain.PermissionAllow},
		},
	}
	if err := ValidatePolicyAgainstCeiling(caps, policy); err != nil {
		t.Fatalf("expected coherent policy to validate, got: %v", err)
	}
}

func TestValidatePolicyAgainstCeiling_AcceptsGranularDeclaration(t *testing.T) {
	// Backward compat: legacy assistants declare granular capabilities
	// instead of family names. Both forms must satisfy the ceiling.
	caps := []string{"filesystem.write", "command.exec"}
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
		},
	}
	if err := ValidatePolicyAgainstCeiling(caps, policy); err != nil {
		t.Fatalf("expected granular declaration to validate, got: %v", err)
	}
}

func TestValidatePolicyAgainstCeiling_AcceptsEmptyCeiling(t *testing.T) {
	// Empty ceiling means "no restriction" per existing runtime semantics.
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
		},
	}
	if err := ValidatePolicyAgainstCeiling(nil, policy); err != nil {
		t.Fatalf("expected empty ceiling to validate, got: %v", err)
	}
}

func TestValidatePolicyAgainstCeiling_RejectsInertConfirmRule(t *testing.T) {
	// The exact case witnessed during V1.3 testing on 2026-04-29: a
	// "Custom" assistant had filesystem.read+web declared but a confirm
	// rule for command.exec — runtime silently denied.
	caps := []string{"filesystem.read", "web"}
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.read", Mode: domain.PermissionConfirm},
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
		},
	}
	err := ValidatePolicyAgainstCeiling(caps, policy)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var cve *CeilingValidationError
	if !errors.As(err, &cve) {
		t.Fatalf("expected *CeilingValidationError, got %T", err)
	}
	if len(cve.Violations) != 2 {
		t.Fatalf("expected 2 violations (filesystem.write, command.exec), got %d: %+v",
			len(cve.Violations), cve.Violations)
	}
	wantCaps := map[string]string{
		"filesystem.write": "filesystem",
		"command.exec":     "command",
	}
	for _, v := range cve.Violations {
		fam, ok := wantCaps[v.Capability]
		if !ok {
			t.Errorf("unexpected violation for capability %q", v.Capability)
			continue
		}
		if v.Family != fam {
			t.Errorf("violation %q: family = %q, want %q", v.Capability, v.Family, fam)
		}
	}
	// SuggestedCapabilities = original ceiling + missing families, sorted.
	want := []string{"command", "filesystem", "filesystem.read", "web"}
	if !equalStrings(cve.SuggestedCapabilities, want) {
		t.Errorf("SuggestedCapabilities = %v, want %v", cve.SuggestedCapabilities, want)
	}
}

func TestValidatePolicyAgainstCeiling_DenyRulesAreExempt(t *testing.T) {
	// A deny rule for an undeclared family is redundant but not
	// misleading — the runtime denies anyway. Don't reject it.
	caps := []string{"filesystem.read"}
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "command.exec", Mode: domain.PermissionDeny},
		},
	}
	if err := ValidatePolicyAgainstCeiling(caps, policy); err != nil {
		t.Fatalf("expected deny rule to be exempt, got: %v", err)
	}
}

func TestValidatePolicyAgainstCeiling_PluginCapabilitiesPassThrough(t *testing.T) {
	// Plugin-scoped caps (gmail.*, github.*) are gated by binding
	// presence, not by the ceiling. They must not trigger validation
	// errors regardless of declared families.
	caps := []string{"filesystem.read"}
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "gmail.send", Mode: domain.PermissionConfirm},
			{Capability: "github.read", Mode: domain.PermissionAllow},
		},
	}
	if err := ValidatePolicyAgainstCeiling(caps, policy); err != nil {
		t.Fatalf("expected plugin caps to pass through, got: %v", err)
	}
}

func TestValidatePolicyAgainstCeiling_PerConnectionOverridesAreChecked(t *testing.T) {
	caps := []string{"filesystem.read"}
	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.read", Mode: domain.PermissionConfirm},
		},
		PerConnectionOverrides: []domain.PerConnectionOverride{
			{ConnectionID: "c1", Capability: "command.exec", Mode: domain.PermissionAllow},
		},
	}
	err := ValidatePolicyAgainstCeiling(caps, policy)
	if err == nil {
		t.Fatal("expected per-connection override to trigger validation error")
	}
	var cve *CeilingValidationError
	if !errors.As(err, &cve) || len(cve.Violations) != 1 {
		t.Fatalf("expected one violation for command.exec override, got: %+v", err)
	}
	if cve.Violations[0].Capability != "command.exec" {
		t.Errorf("violation capability = %q, want command.exec", cve.Violations[0].Capability)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
