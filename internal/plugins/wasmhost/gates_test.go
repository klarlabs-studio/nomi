package wasmhost

import (
	"errors"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/permissions"
)

// Each gate failure mode covered by an isolated test. The four
// security layers (manifest declaration, system-context rejection,
// policy deny, constraint mismatch) each have distinct failure modes
// the install dialog will surface differently to users — tests here
// pin the diagnostic strings so future refactors don't accidentally
// swallow the "why" of a denied call.

func TestGate_RejectsCapabilityNotInManifest(t *testing.T) {
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		Capabilities: []string{"network.outgoing"}, // doesn't declare command.exec
		Policy:       allowAllPolicy(),
		Engine:       permissions.NewEngine(),
	}
	_, err := gate(cfg, "command.exec")
	if err == nil {
		t.Fatal("expected denial for undeclared capability")
	}
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected ErrCapabilityDenied wrap, got %v", err)
	}
}

func TestGate_RejectsSystemContextCalls(t *testing.T) {
	// AssistantID empty = no policy applies = deny. Defense-in-depth:
	// even if a plugin declared the capability, system-context calls
	// must go through an explicit assistant identity.
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "",
		Capabilities: []string{"network.outgoing"},
	}
	_, err := gate(cfg, "network.outgoing")
	if err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected denial for system-context call, got %v", err)
	}
}

func TestGate_RejectsPolicyDeny(t *testing.T) {
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		Capabilities: []string{"network.outgoing"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "network.outgoing", Mode: domain.PermissionDeny},
			},
		},
		Engine: permissions.NewEngine(),
	}
	_, err := gate(cfg, "network.outgoing")
	if err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected denial under PermissionDeny rule, got %v", err)
	}
}

func TestGate_RejectsConfirmModeUntilApprovalBridgeLands(t *testing.T) {
	// Confirm requires the runtime's approval flow which the WASM host
	// hasn't bridged yet (lifecycle-07 work). v1 contract: confirm =
	// deny so a malicious plugin can't smuggle past confirm-gated
	// capabilities.
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		Capabilities: []string{"network.outgoing"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "network.outgoing", Mode: domain.PermissionConfirm},
			},
		},
		Engine: permissions.NewEngine(),
	}
	_, err := gate(cfg, "network.outgoing")
	if err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected confirm-mode rejection, got %v", err)
	}
}

func TestGate_AllowsWhenAllLayersPass(t *testing.T) {
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		Capabilities: []string{"network.outgoing"},
		Policy:       allowAllPolicy(),
		Engine:       permissions.NewEngine(),
	}
	rule, err := gate(cfg, "network.outgoing")
	if err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if rule == nil {
		t.Fatal("rule should be returned for constraint inspection")
	}
}

func TestGate_PerConnectionOverrideTakesPrecedence(t *testing.T) {
	// Per ADR 0001 §7: per-connection overrides resolve before
	// per-capability rules. Verify the gate consults
	// MatchingOverrideOrRule (not just MatchingRule) so the override
	// path is honored.
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		ConnectionID: "conn-work",
		Capabilities: []string{"network.outgoing"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "network.outgoing", Mode: domain.PermissionAllow},
			},
			PerConnectionOverrides: []domain.PerConnectionOverride{
				{ConnectionID: "conn-work", Capability: "network.outgoing", Mode: domain.PermissionDeny},
			},
		},
		Engine: permissions.NewEngine(),
	}
	_, err := gate(cfg, "network.outgoing")
	if err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("per-connection override should deny, got %v", err)
	}
}

// --- network host-allowlist tests ---

func TestHostAllowed_BothEmptyDenies(t *testing.T) {
	if hostAllowed("api.slack.com", nil, nil) {
		t.Fatal("empty manifest + empty policy must deny (no allowlist = no permission)")
	}
}

func TestHostAllowed_ManifestExactMatch(t *testing.T) {
	if !hostAllowed("api.slack.com", []string{"api.slack.com"}, nil) {
		t.Fatal("manifest exact match should allow")
	}
}

func TestHostAllowed_WildcardSubdomainMatch(t *testing.T) {
	if !hostAllowed("files.slack.com", []string{"*.slack.com"}, nil) {
		t.Fatal("wildcard should match subdomain")
	}
	if hostAllowed("slack.com.attacker.com", []string{"*.slack.com"}, nil) {
		t.Fatal("wildcard must not match suffix-only impersonation (leading-dot anchor)")
	}
}

func TestHostAllowed_IntersectionNotUnion(t *testing.T) {
	// host must satisfy BOTH manifest and policy when both are non-empty.
	allowed := hostAllowed(
		"api.slack.com",
		[]string{"api.slack.com", "*.slack.com"},
		[]string{"api.gmail.com"},
	)
	if allowed {
		t.Fatal("manifest allows but policy doesn't — intersection should deny")
	}
}

func TestStripPort_HandlesWithAndWithoutPort(t *testing.T) {
	cases := map[string]string{
		"api.slack.com":     "api.slack.com",
		"api.slack.com:443": "api.slack.com",
		"localhost:8080":    "localhost",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Fatalf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGateCommand_RejectsUnlistedBinary(t *testing.T) {
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		Capabilities: []string{"command.exec"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "command.exec", Mode: domain.PermissionAllow,
					Constraints: map[string]interface{}{
						"allowed_binaries": []string{"git", "make"},
					}},
			},
		},
		Engine: permissions.NewEngine(),
	}
	if err := gateCommand(cfg, "rm"); err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("unlisted binary should be denied, got %v", err)
	}
	if err := gateCommand(cfg, "git"); err != nil {
		t.Fatalf("listed binary should be allowed, got %v", err)
	}
}

func TestGateCommand_RejectsEmptyAllowlist(t *testing.T) {
	// An assistant with command.exec but no allowed_binaries is a
	// policy bug — better to refuse than to interpret as "anything
	// goes." Tested explicitly so we don't accidentally relax this.
	cfg := &CallConfig{
		PluginID:     "com.example.test",
		AssistantID:  "asst-1",
		Capabilities: []string{"command.exec"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "command.exec", Mode: domain.PermissionAllow},
			},
		},
		Engine: permissions.NewEngine(),
	}
	if err := gateCommand(cfg, "git"); err == nil {
		t.Fatal("empty allowed_binaries should refuse")
	}
}

func TestStringSliceFromConstraint_HandlesBothShapes(t *testing.T) {
	// JSON unmarshal produces []interface{} not []string. Both must
	// work because the constraint may arrive either way depending on
	// whether the policy was loaded from the DB (interface{}) or
	// constructed in Go directly (string).
	asInterface := map[string]interface{}{"x": []interface{}{"a", "b"}}
	asString := map[string]interface{}{"x": []string{"a", "b"}}
	for _, m := range []map[string]interface{}{asInterface, asString} {
		got := stringSliceFromConstraint(m, "x")
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("got %v", got)
		}
	}
}

// allowAllPolicy is a test fixture so individual cases stay short.
func allowAllPolicy() *domain.PermissionPolicy {
	return &domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "*", Mode: domain.PermissionAllow},
		},
	}
}
