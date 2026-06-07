package permissions

import (
	"fmt"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// SystemCapabilityFamilies maps a permission capability string to the
// declared-capability family it belongs to. The assistant builder UI
// surfaces the families as checkboxes ("Filesystem", "Command", "Web");
// the runtime uses this map to decide whether a given capability call
// is permitted by the assistant's declared ceiling.
//
// Plugin-tool capabilities (gmail.*, calendar.*, browser.*, echo.*) are
// intentionally absent — those are gated by assistant_connection_bindings
// presence, not by the ceiling. That keeps "add a Gmail connection"
// from also requiring "edit the agent's declared capabilities" — the
// binding IS the grant for plugin tools.
var SystemCapabilityFamilies = map[string]string{
	"filesystem.read":    "filesystem",
	"filesystem.write":   "filesystem",
	"filesystem.context": "filesystem",
	"command.exec":       "command",
	"network.outgoing":   "web",
}

// ImplicitCapability names a capability the runtime never asks the
// user to declare explicitly. llm.chat is the only one — every
// assistant needs the planner step, requiring users to remember to
// tick a "Chat" box would be friction without value.
const ImplicitCapability = "llm.chat"

// DeclaredCapabilityCeiling reports whether the assistant's declared
// capability list permits a given capability. Returns true when:
//   - declared is empty (legacy / unconfigured → no ceiling)
//   - capability is the implicit llm.chat
//   - capability is plugin-scoped (gated by bindings, not ceiling)
//   - declared contains the capability's family name (UI form: "filesystem")
//   - declared contains the granular capability (e.g. "filesystem.read")
//     for backward compat with assistants created before the ceiling
//     was wired
func DeclaredCapabilityCeiling(declared []string, capability string) bool {
	if len(declared) == 0 {
		return true
	}
	if capability == ImplicitCapability {
		return true
	}
	family, isSystem := SystemCapabilityFamilies[capability]
	if !isSystem {
		return true
	}
	for _, d := range declared {
		if d == family || d == capability {
			return true
		}
	}
	return false
}

// CeilingViolation describes a single inert policy rule — a non-deny
// rule whose capability family is not in the declared ceiling. Surfaced
// to the API so clients can render an actionable error rather than the
// rule silently failing at runtime.
type CeilingViolation struct {
	Capability string `json:"capability"`
	Family     string `json:"family"`
}

// CeilingValidationError is returned from ValidatePolicyAgainstCeiling
// when one or more rules would have no effect because their family is
// not declared. Includes the suggested capabilities list (declared +
// the missing families) so a UI can offer a one-click "Add families"
// fix without re-deriving the set itself.
type CeilingValidationError struct {
	Violations            []CeilingViolation `json:"violations"`
	SuggestedCapabilities []string           `json:"suggested_capabilities"`
}

func (e *CeilingValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, fmt.Sprintf("%q (needs family %q)", v.Capability, v.Family))
	}
	return fmt.Sprintf(
		"policy contains %d rule(s) with no effect because their capability family is not declared: %s — add the family to capabilities or remove the rule",
		len(e.Violations), strings.Join(parts, ", "),
	)
}

// ValidatePolicyAgainstCeiling rejects assistants where a non-deny
// policy rule references a capability whose family is not in the
// declared ceiling. Such rules are silently ineffective at runtime —
// the ceiling check fires first and denies, so the user sees
// "permission denied" with no breadcrumb pointing at the missing
// checkbox in the builder. Validating at the API boundary makes the
// failure loud at write time, when the user can still fix it.
//
// Deny rules are intentionally exempt: a "deny" rule for an undeclared
// family is redundant but not misleading — the runtime denies anyway.
func ValidatePolicyAgainstCeiling(capabilities []string, policy domain.PermissionPolicy) error {
	violations := []CeilingViolation{}
	missingFamilies := map[string]struct{}{}

	check := func(cap string, mode domain.PermissionMode) {
		if mode == domain.PermissionDeny {
			return
		}
		if DeclaredCapabilityCeiling(capabilities, cap) {
			return
		}
		family, ok := SystemCapabilityFamilies[cap]
		if !ok {
			// Plugin-scoped capability — should never reach here per
			// DeclaredCapabilityCeiling's plugin-passthrough, but guard
			// against a future map change that introduces a false
			// negative for unknown caps.
			return
		}
		violations = append(violations, CeilingViolation{Capability: cap, Family: family})
		missingFamilies[family] = struct{}{}
	}

	for _, rule := range policy.Rules {
		check(rule.Capability, rule.Mode)
	}
	for _, override := range policy.PerConnectionOverrides {
		check(override.Capability, override.Mode)
	}

	if len(violations) == 0 {
		return nil
	}

	suggested := append([]string{}, capabilities...)
	for fam := range missingFamilies {
		suggested = append(suggested, fam)
	}
	sort.Strings(suggested)

	return &CeilingValidationError{
		Violations:            violations,
		SuggestedCapabilities: suggested,
	}
}
