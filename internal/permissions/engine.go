package permissions

import (
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// Engine evaluates permissions and manages approval requests
type Engine struct {
	// Add any dependencies here (e.g., approval store)
}

// NewEngine creates a new permission engine
func NewEngine() *Engine {
	return &Engine{}
}

// Evaluate evaluates a capability against a policy. Exact matches win over
// wildcards, and the most specific wildcard (longest prefix) wins over a
// less specific one. Unmatched capabilities are denied.
//
// Per-connection overrides are applied via EvaluateForConnection; this
// entry point assumes "no specific connection" and resolves against
// Rules only.
func (e *Engine) Evaluate(policy domain.PermissionPolicy, capability string) domain.PermissionMode {
	if rule := e.MatchingRule(policy, capability); rule != nil {
		return rule.Mode
	}
	return domain.PermissionDeny
}

// EvaluateForConnection evaluates a capability against a policy,
// consulting per-connection overrides first. Per ADR 0001 §7:
// per-connection override → matching PermissionRule → implicit deny.
//
// connectionID may be empty, in which case behavior is identical to
// Evaluate. This lets callers use one code path regardless of whether
// the capability is tied to a specific plugin Connection.
func (e *Engine) EvaluateForConnection(policy domain.PermissionPolicy, capability, connectionID string) domain.PermissionMode {
	if connectionID != "" {
		if ov := findOverride(policy, connectionID, capability); ov != nil {
			return ov.Mode
		}
	}
	return e.Evaluate(policy, capability)
}

// MatchingOverrideOrRule returns either a per-connection override or a
// matching rule — whichever would apply under EvaluateForConnection. Used
// by callers that need to read constraints (e.g. allowed_binaries) in
// addition to the mode.
//
// The returned PermissionRule is always a value copy (overrides don't
// share the same struct as Rules), so callers can mutate the result
// safely.
func (e *Engine) MatchingOverrideOrRule(policy domain.PermissionPolicy, capability, connectionID string) *domain.PermissionRule {
	if connectionID != "" {
		if ov := findOverride(policy, connectionID, capability); ov != nil {
			return &domain.PermissionRule{
				Capability:  ov.Capability,
				Mode:        ov.Mode,
				Constraints: ov.Constraints,
			}
		}
	}
	return e.MatchingRule(policy, capability)
}

// findOverride searches per-connection overrides for an exact
// (connection_id, capability) match. Wildcards on the capability are
// NOT supported in overrides — an override should be narrow by design;
// if a user wants a wildcard-level change for a connection, they can
// add multiple overrides.
func findOverride(policy domain.PermissionPolicy, connectionID, capability string) *domain.PerConnectionOverride {
	for i := range policy.PerConnectionOverrides {
		ov := &policy.PerConnectionOverrides[i]
		if ov.ConnectionID == connectionID && ov.Capability == capability {
			return ov
		}
	}
	return nil
}

// MatchingRule returns the PermissionRule that would apply to the given
// capability under this policy, or nil if no rule matches. The same
// exact-then-longest-wildcard precedence as Evaluate. Callers use this to
// read per-rule Constraints (e.g. allowed_binaries for command.exec) in
// addition to the raw mode.
//
// The returned pointer is into the policy's own slice; callers must not
// mutate it.
func (e *Engine) MatchingRule(policy domain.PermissionPolicy, capability string) *domain.PermissionRule {
	var (
		best       *domain.PermissionRule
		bestLength int
	)

	for i, rule := range policy.Rules {
		if rule.Capability == capability {
			// Exact match wins immediately; no need to keep looking.
			return &policy.Rules[i]
		}
		if !matchWildcard(rule.Capability, capability) {
			continue
		}
		// Longer patterns are more specific: "filesystem.write" beats
		// "filesystem.*" beats "*".
		if best == nil || len(rule.Capability) > bestLength {
			best = &policy.Rules[i]
			bestLength = len(rule.Capability)
		}
	}
	return best
}

// EvaluateWithContext is retained for callers that may pass structured
// context in the future (e.g., attack surface attribution). Today context is
// unused; the evaluation is identical to Evaluate.
func (e *Engine) EvaluateWithContext(policy domain.PermissionPolicy, capability string, _ map[string]interface{}) domain.PermissionMode {
	return e.Evaluate(policy, capability)
}

// ValidatePolicy validates a permission policy
func (e *Engine) ValidatePolicy(policy domain.PermissionPolicy) error {
	seen := make(map[string]bool)
	for i, rule := range policy.Rules {
		if rule.Capability == "" {
			return fmt.Errorf("rule %d: capability is required", i)
		}
		if !rule.Mode.IsValid() {
			return fmt.Errorf("rule %d: invalid mode '%s'", i, rule.Mode)
		}
		if seen[rule.Capability] {
			return fmt.Errorf("rule %d: duplicate capability '%s'", i, rule.Capability)
		}
		seen[rule.Capability] = true
	}
	return nil
}

// BuildDefaultPolicy creates a default permission policy
func BuildDefaultPolicy() domain.PermissionPolicy {
	return domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			// llm.chat defaults to allow: without it the agent can't think,
			// and the provider config already gates which model + endpoint
			// the call routes to. Per-assistant policy can tighten this.
			{Capability: "llm.chat", Mode: domain.PermissionAllow},
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
			{Capability: "network.*", Mode: domain.PermissionDeny},
		},
	}
}

// BuildPermissivePolicy creates a permissive policy (for testing/development)
func BuildPermissivePolicy() domain.PermissionPolicy {
	return domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "*", Mode: domain.PermissionAllow},
		},
	}
}

// BuildRestrictedPolicy creates a restricted policy
func BuildRestrictedPolicy() domain.PermissionPolicy {
	return domain.PermissionPolicy{
		Rules: []domain.PermissionRule{
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "filesystem.write", Mode: domain.PermissionDeny},
			{Capability: "command.exec", Mode: domain.PermissionDeny},
			{Capability: "network.*", Mode: domain.PermissionDeny},
		},
	}
}

// DefaultSafetyProfile is the global safety profile applied to fresh
// installs and to any setting lookup that returns an empty value. Balanced
// trades off "let the assistant make progress without nagging on every
// llm.chat" against "still confirm anything that touches the filesystem,
// shell, or network." Cautious is reachable from the Settings tab and was
// the original default; we changed it because cautious blocks llm.chat by
// default which is the wrong first impression of a chat-based product.
const DefaultSafetyProfile = "balanced"

// ValidSafetyProfiles enumerates the names BuildSafetyProfilePolicy
// recognises. Callers (e.g. the HTTP layer) use this to validate input
// instead of duplicating the literal list, so adding a fourth profile is
// a one-place change.
func ValidSafetyProfiles() []string {
	return []string{"cautious", "balanced", "fast"}
}

// IsValidSafetyProfile reports whether the supplied name is one of the
// recognised profiles. Whitespace and case-insensitivity are intentionally
// not handled here; the API surface stores the canonical lowercase form.
func IsValidSafetyProfile(name string) bool {
	for _, p := range ValidSafetyProfiles() {
		if p == name {
			return true
		}
	}
	return false
}

// BuildSafetyProfilePolicy maps a global safety profile to a default
// assistant permission policy.
func BuildSafetyProfilePolicy(profile string) domain.PermissionPolicy {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "fast":
		return domain.PermissionPolicy{Rules: []domain.PermissionRule{
			{Capability: "llm.chat", Mode: domain.PermissionAllow},
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "filesystem.write", Mode: domain.PermissionAllow},
			{Capability: "command.exec", Mode: domain.PermissionAllow},
			{Capability: "network.outgoing", Mode: domain.PermissionConfirm},
		}}
	case "balanced":
		return domain.PermissionPolicy{Rules: []domain.PermissionRule{
			{Capability: "llm.chat", Mode: domain.PermissionAllow},
			{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
			{Capability: "network.outgoing", Mode: domain.PermissionConfirm},
		}}
	default: // cautious
		return domain.PermissionPolicy{Rules: []domain.PermissionRule{
			{Capability: "llm.chat", Mode: domain.PermissionConfirm},
			{Capability: "filesystem.read", Mode: domain.PermissionConfirm},
			{Capability: "filesystem.write", Mode: domain.PermissionConfirm},
			{Capability: "command.exec", Mode: domain.PermissionConfirm},
			{Capability: "network.outgoing", Mode: domain.PermissionConfirm},
		}}
	}
}

// matchWildcard checks if a capability pattern matches a concrete capability.
// Supported forms:
//
//	"filesystem.read" — literal, exact match only
//	"filesystem.*"    — matches any capability under filesystem.*
//	"*"               — matches any capability
//
// The leading-dot anchor (requiring a "." between prefix and suffix) is
// important: "file.*" must not swallow "files.write" — capabilities are
// dotted namespaces, not raw string prefixes.
func matchWildcard(pattern, capability string) bool {
	if pattern == capability {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := strings.TrimSuffix(pattern, ".*")
		return strings.HasPrefix(capability, prefix+".")
	}
	return false
}

// MergePolicies merges multiple policies, with later policies taking precedence
func MergePolicies(policies ...domain.PermissionPolicy) domain.PermissionPolicy {
	merged := domain.PermissionPolicy{
		Rules: make([]domain.PermissionRule, 0),
	}
	seen := make(map[string]bool)

	// Process in reverse to give precedence to later policies
	for i := len(policies) - 1; i >= 0; i-- {
		for _, rule := range policies[i].Rules {
			if !seen[rule.Capability] {
				merged.Rules = append(merged.Rules, rule)
				seen[rule.Capability] = true
			}
		}
	}

	return merged
}
