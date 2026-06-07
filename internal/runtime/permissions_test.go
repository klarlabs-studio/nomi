package runtime

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/permissions"
)

// declaredCapabilityCeiling is the gate the assistant builder's
// "Declared capabilities" checkboxes drive. The tests here pin down
// every contract corner so a future refactor can't accidentally
// loosen the ceiling without surfacing the change.

func TestDeclaredCapabilityCeiling_EmptyDeclaredAllowsEverything(t *testing.T) {
	// Backward compat: assistants created before the ceiling was wired
	// have empty Capabilities. Treating empty as "no ceiling" keeps
	// them working without a migration.
	for _, cap := range []string{"filesystem.read", "command.exec", "gmail.send", "anything"} {
		if !declaredCapabilityCeiling(nil, cap) {
			t.Errorf("empty ceiling should allow %q", cap)
		}
	}
}

func TestDeclaredCapabilityCeiling_LLMChatIsImplicit(t *testing.T) {
	// llm.chat is the planner step — every assistant uses it.
	// Forcing users to remember to check a "Chat" box would be
	// friction without value.
	if !declaredCapabilityCeiling([]string{"filesystem"}, "llm.chat") {
		t.Fatal("llm.chat must be implicit even when ceiling is declared")
	}
}

func TestDeclaredCapabilityCeiling_PluginCapsBypassCeiling(t *testing.T) {
	// gmail.send / browser.navigate / calendar.read etc. are gated
	// by assistant_connection_bindings, not by the ceiling. The
	// builder's checkboxes don't list them and shouldn't.
	declared := []string{"filesystem"} // strict ceiling
	for _, cap := range []string{"gmail.send", "browser.navigate", "calendar.read", "echo.echo", "discord.post"} {
		if !declaredCapabilityCeiling(declared, cap) {
			t.Errorf("plugin capability %q should bypass ceiling (gated by bindings)", cap)
		}
	}
}

func TestDeclaredCapabilityCeiling_FamilyMatch(t *testing.T) {
	// UI checkboxes hand back family names — "filesystem" matches
	// every filesystem.* capability.
	declared := []string{"filesystem"}
	for _, cap := range []string{"filesystem.read", "filesystem.write", "filesystem.context"} {
		if !declaredCapabilityCeiling(declared, cap) {
			t.Errorf("filesystem family should permit %q", cap)
		}
	}
	// command + web map to single capabilities.
	if !declaredCapabilityCeiling([]string{"command"}, "command.exec") {
		t.Error("command family should permit command.exec")
	}
	if !declaredCapabilityCeiling([]string{"web"}, "network.outgoing") {
		t.Error("web family should permit network.outgoing")
	}
}

func TestDeclaredCapabilityCeiling_GranularBackwardCompat(t *testing.T) {
	// Assistants created via API with granular capability strings
	// (e.g. "filesystem.read" rather than the family "filesystem")
	// must keep working — the e2e test from the live walk creates
	// assistants this way.
	for _, cap := range []string{"filesystem.read", "command.exec", "network.outgoing"} {
		if !declaredCapabilityCeiling([]string{cap}, cap) {
			t.Errorf("granular declaration %q should permit itself", cap)
		}
	}
}

func TestDeclaredCapabilityCeiling_DeniesUndeclaredFamily(t *testing.T) {
	// The whole point: an assistant with only "filesystem" declared
	// must be refused command.exec even if a permission rule says
	// allow. The test asserts the ceiling is strict-by-default once
	// declared.
	declared := []string{"filesystem"}
	for _, cap := range []string{"command.exec", "network.outgoing"} {
		if declaredCapabilityCeiling(declared, cap) {
			t.Errorf("ceiling %v should deny %q", declared, cap)
		}
	}
}

func TestDeclaredCapabilityCeiling_FamilyAndGranularMix(t *testing.T) {
	// A sane future state: UI lets users tick "filesystem" AND
	// add a granular "command.exec" via the Permissions section.
	// Both forms in the same list should resolve correctly.
	declared := []string{"filesystem", "command.exec"}
	if !declaredCapabilityCeiling(declared, "filesystem.read") {
		t.Error("family entry should still match granular cap")
	}
	if !declaredCapabilityCeiling(declared, "command.exec") {
		t.Error("granular entry should still match itself")
	}
	if declaredCapabilityCeiling(declared, "network.outgoing") {
		t.Error("undeclared web family should still deny")
	}
}

// --- effectivePermissionMode: ceiling overrides allow rules --------

func newRuntimeForCeilingTest(t *testing.T) *Runtime {
	t.Helper()
	// Minimal runtime for the permission resolver — only needs the
	// permission engine wired. No DB, no event bus.
	return &Runtime{permEngine: permissions.NewEngine()}
}

func TestEffectivePermissionMode_CeilingOverridesAllowRule(t *testing.T) {
	// Assistant has filesystem in the ceiling but command isn't
	// declared. Even with a wide-open command.exec=allow rule, the
	// runtime must deny.
	rt := newRuntimeForCeilingTest(t)
	asst := &domain.AssistantDefinition{
		Capabilities: []string{"filesystem"},
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "*", Mode: domain.PermissionAllow},
			},
		},
	}
	if got := rt.effectivePermissionMode(nil, asst, "command.exec"); got != domain.PermissionDeny {
		t.Fatalf("ceiling violation should deny, got %s", got)
	}
}

func TestEffectivePermissionMode_CeilingPermitsDeclaredFamily(t *testing.T) {
	rt := newRuntimeForCeilingTest(t)
	asst := &domain.AssistantDefinition{
		Capabilities: []string{"filesystem"},
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			},
		},
	}
	if got := rt.effectivePermissionMode(nil, asst, "filesystem.read"); got != domain.PermissionAllow {
		t.Fatalf("declared+allowed cap should resolve to allow, got %s", got)
	}
}

func TestEffectivePermissionMode_PluginCapNotSubjectToCeiling(t *testing.T) {
	// Plugin capability (echo.echo) when ceiling is strict on
	// system caps — should pass through to the policy mode without
	// the ceiling interfering.
	rt := newRuntimeForCeilingTest(t)
	asst := &domain.AssistantDefinition{
		Capabilities: []string{"filesystem"}, // declared but doesn't include echo.echo
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "echo.echo", Mode: domain.PermissionAllow},
			},
		},
	}
	if got := rt.effectivePermissionMode(nil, asst, "echo.echo"); got != domain.PermissionAllow {
		t.Fatalf("plugin capability should pass ceiling, got %s", got)
	}
}
