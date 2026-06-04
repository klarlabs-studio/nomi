package agent

import (
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/permissions"
)

// Mode is a capability evaluation result. It mirrors Nomi's permission modes.
type Mode string

const (
	ModeAllow   Mode = "allow"
	ModeConfirm Mode = "confirm"
	ModeDeny    Mode = "deny"
)

// Rule grants a capability a mode. Capabilities may use wildcards (e.g.
// "network.*"), evaluated by Nomi's canonical permission engine.
type Rule struct {
	Capability string
	Mode       Mode
}

// Policy is the set of rules a run is gated against. An unmatched capability is
// implicitly denied (secure by default).
type Policy struct {
	Rules []Rule
}

// AllowOnly is a convenience constructor: every listed capability is allowed,
// everything else denied.
func AllowOnly(capabilities ...string) Policy {
	rules := make([]Rule, 0, len(capabilities))
	for _, c := range capabilities {
		rules = append(rules, Rule{Capability: c, Mode: ModeAllow})
	}
	return Policy{Rules: rules}
}

// sharedEngine is the canonical, stateless permission engine.
var sharedEngine = permissions.NewEngine()

// evaluate resolves a capability to a mode using Nomi's permission engine,
// inheriting its wildcard matching and implicit-deny semantics.
func (p Policy) evaluate(capability string) Mode {
	return Mode(sharedEngine.Evaluate(p.toDomain(), capability))
}

func (p Policy) toDomain() domain.PermissionPolicy {
	rules := make([]domain.PermissionRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, domain.PermissionRule{
			Capability: r.Capability,
			Mode:       domain.PermissionMode(r.Mode),
		})
	}
	return domain.PermissionPolicy{Rules: rules}
}
