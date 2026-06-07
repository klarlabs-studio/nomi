package permissions

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestEngineEvaluate(t *testing.T) {
	engine := NewEngine()

	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionDeny},
			{Capability: "network.*", Mode: domain.PermissionDeny},
		},
	}

	tests := []struct {
		name       string
		capability string
		want       domain.PermissionMode
	}{
		{"allow exact match", "filesystem.read", domain.PermissionAllow},
		{"confirm exact match", "filesystem.write", domain.PermissionConfirm},
		{"deny exact match", "command.exec", domain.PermissionDeny},
		{"wildcard deny", "network.http", domain.PermissionDeny},
		{"wildcard deny 2", "network.telegram", domain.PermissionDeny},
		{"default deny", "unknown.capability", domain.PermissionDeny},
		{"empty policy deny", "", domain.PermissionDeny},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := engine.Evaluate(policy, tt.capability)
			if got != tt.want {
				t.Errorf("Evaluate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEngineEvaluateWithContext(t *testing.T) {
	engine := NewEngine()

	policy := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "filesystem.*", Mode: domain.PermissionConfirm},
		},
	}

	// Exact match should take precedence over wildcard
	got := engine.EvaluateWithContext(policy, "filesystem.read", nil)
	if got != domain.PermissionAllow {
		t.Errorf("EvaluateWithContext() = %v, want %v", got, domain.PermissionAllow)
	}

	// Wildcard match
	got = engine.EvaluateWithContext(policy, "filesystem.write", nil)
	if got != domain.PermissionConfirm {
		t.Errorf("EvaluateWithContext() = %v, want %v", got, domain.PermissionConfirm)
	}
}

func TestValidatePolicy(t *testing.T) {
	engine := NewEngine()

	tests := []struct {
		name    string
		policy  domain.PermissionPolicy
		wantErr bool
	}{
		{
			name: "valid policy",
			policy: domain.PermissionPolicy{
				Rules: []domain.PermissionRule{
					{Capability: "filesystem.read", Mode: domain.PermissionAllow},
				},
			},
			wantErr: false,
		},
		{
			name: "empty capability",
			policy: domain.PermissionPolicy{
				Rules: []domain.PermissionRule{
					{Capability: "", Mode: domain.PermissionAllow},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid mode",
			policy: domain.PermissionPolicy{
				Rules: []domain.PermissionRule{
					{Capability: "test", Mode: "invalid"},
				},
			},
			wantErr: true,
		},
		{
			name: "duplicate capability",
			policy: domain.PermissionPolicy{
				Rules: []domain.PermissionRule{
					{Capability: "test", Mode: domain.PermissionAllow},
					{Capability: "test", Mode: domain.PermissionDeny},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := engine.ValidatePolicy(tt.policy)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePolicy() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMatchWildcard(t *testing.T) {
	tests := []struct {
		pattern    string
		capability string
		want       bool
	}{
		{"filesystem.*", "filesystem.read", true},
		{"filesystem.*", "filesystem.write", true},
		{"filesystem.*", "network.http", false},
		{"*", "anything.goes", true},
		{"exact.match", "exact.match", true},
		{"exact.match", "exact.match2", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.capability, func(t *testing.T) {
			got := matchWildcard(tt.pattern, tt.capability)
			if got != tt.want {
				t.Errorf("matchWildcard(%q, %q) = %v, want %v", tt.pattern, tt.capability, got, tt.want)
			}
		})
	}
}

func TestMergePolicies(t *testing.T) {
	policy1 := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "command.exec", Mode: domain.PermissionDeny},
		},
	}

	policy2 := domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
			{Capability: "network.http", Mode: domain.PermissionAllow},
		},
	}

	merged := MergePolicies(policy1, policy2)

	// policy2 should take precedence for command.exec
	engine := NewEngine()
	mode := engine.Evaluate(merged, "command.exec")
	if mode != domain.PermissionConfirm {
		t.Errorf("merged policy for command.exec = %v, want %v", mode, domain.PermissionConfirm)
	}

	// filesystem.read should still be allow
	mode = engine.Evaluate(merged, "filesystem.read")
	if mode != domain.PermissionAllow {
		t.Errorf("merged policy for filesystem.read = %v, want %v", mode, domain.PermissionAllow)
	}

	// network.http should be added
	mode = engine.Evaluate(merged, "network.http")
	if mode != domain.PermissionAllow {
		t.Errorf("merged policy for network.http = %v, want %v", mode, domain.PermissionAllow)
	}
}

func TestBuildPolicies(t *testing.T) {
	engine := NewEngine()

	// Test default policy
	defaultPolicy := BuildDefaultPolicy()
	if err := engine.ValidatePolicy(defaultPolicy); err != nil {
		t.Errorf("BuildDefaultPolicy() invalid: %v", err)
	}

	// Test permissive policy
	permissive := BuildPermissivePolicy()
	mode := engine.Evaluate(permissive, "anything.goes")
	if mode != domain.PermissionAllow {
		t.Errorf("BuildPermissivePolicy() should allow everything, got %v", mode)
	}

	// Test restricted policy
	restricted := BuildRestrictedPolicy()
	mode = engine.Evaluate(restricted, "command.exec")
	if mode != domain.PermissionDeny {
		t.Errorf("BuildRestrictedPolicy() should deny command.exec, got %v", mode)
	}
}

func TestBuildSafetyProfilePolicy(t *testing.T) {
	engine := NewEngine()

	cautious := BuildSafetyProfilePolicy("cautious")
	if got := engine.Evaluate(cautious, "filesystem.read"); got != domain.PermissionConfirm {
		t.Fatalf("cautious filesystem.read = %v, want confirm", got)
	}

	balanced := BuildSafetyProfilePolicy("balanced")
	if got := engine.Evaluate(balanced, "filesystem.read"); got != domain.PermissionAllow {
		t.Fatalf("balanced filesystem.read = %v, want allow", got)
	}
	if got := engine.Evaluate(balanced, "command.exec"); got != domain.PermissionConfirm {
		t.Fatalf("balanced command.exec = %v, want confirm", got)
	}

	fast := BuildSafetyProfilePolicy("fast")
	if got := engine.Evaluate(fast, "filesystem.write"); got != domain.PermissionAllow {
		t.Fatalf("fast filesystem.write = %v, want allow", got)
	}
}
