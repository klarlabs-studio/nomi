package runtime

import (
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/permissions"
)

// declaredCapabilityCeiling is a thin alias kept so the runtime's call
// sites read naturally. The canonical implementation lives in
// internal/permissions so the API layer can validate writes against the
// same rule set the runtime enforces at execution time.
func declaredCapabilityCeiling(declared []string, capability string) bool {
	return permissions.DeclaredCapabilityCeiling(declared, capability)
}

// effectivePermissionMode resolves the capability check for a step in its
// run's context. For a desktop-sourced run (Source == nil) the assistant
// policy is authoritative. For a connector-sourced run, the assistant policy
// is intersected with the connector manifest: the capability must appear in
// the manifest or the check is denied. This means a connector that declared
// only `network.outgoing` cannot make the runtime execute `command.exec`,
// regardless of how permissive the assistant's policy is.
//
// The assistant's declared Capabilities ceiling is checked FIRST: if the
// assistant didn't declare the capability family in its builder, the
// runtime denies regardless of how permissive the policy rules are. This
// matches what users expect when they uncheck a box in the assistant
// builder — the capability stops working immediately, even if a stale
// policy rule still says allow.
func (r *Runtime) effectivePermissionMode(run *domain.Run, assistant *domain.AssistantDefinition, capability string) domain.PermissionMode {
	if !declaredCapabilityCeiling(assistant.Capabilities, capability) {
		return domain.PermissionDeny
	}
	assistantMode := r.permEngine.Evaluate(assistant.PermissionPolicy, capability)

	if run == nil || run.Source == nil || *run.Source == "" {
		return assistantMode
	}
	source := *run.Source

	if r.connectorManifest == nil {
		// Unknown enforcement surface; fail closed.
		return domain.PermissionDeny
	}
	declared, ok := r.connectorManifest(source)
	if !ok {
		// Connector was deregistered between run creation and execution;
		// refuse rather than grant.
		return domain.PermissionDeny
	}

	// Connectors declare capability patterns the same way assistant policies
	// do (e.g. "filesystem.*"). Build a minimal policy from the declared set
	// with Allow, then intersect: both must be at least Confirm for the
	// effective mode to be Confirm; both must be Allow to be Allow; any Deny
	// wins.
	manifestPolicy := domain.PermissionPolicy{
		Rules: make([]domain.PermissionRule, 0, len(declared)),
	}
	for _, cap := range declared {
		manifestPolicy.Rules = append(manifestPolicy.Rules, domain.PermissionRule{
			Capability: cap,
			Mode:       domain.PermissionAllow,
		})
	}
	manifestMode := r.permEngine.Evaluate(manifestPolicy, capability)

	return intersectModes(assistantMode, manifestMode)
}

// intersectModes returns the strictest of two permission modes.
// Deny dominates Confirm dominates Allow: a capability is only allowed
// outright when both sources agree to allow it.
func intersectModes(a, b domain.PermissionMode) domain.PermissionMode {
	if a == domain.PermissionDeny || b == domain.PermissionDeny {
		return domain.PermissionDeny
	}
	if a == domain.PermissionConfirm || b == domain.PermissionConfirm {
		return domain.PermissionConfirm
	}
	return domain.PermissionAllow
}
