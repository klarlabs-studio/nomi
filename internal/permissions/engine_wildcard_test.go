package permissions

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

// TestEvaluateExactOverridesWildcard asserts that an exact-match rule wins
// over any wildcard rule, regardless of declaration order.
func TestEvaluateExactOverridesWildcard(t *testing.T) {
	e := NewEngine()

	cases := []struct {
		name   string
		policy domain.PermissionPolicy
		cap    string
		want   domain.PermissionMode
	}{
		{
			name: "exact allow beats wildcard deny",
			policy: domain.PermissionPolicy{Rules: []domain.PermissionRule{
				{Capability: "filesystem.*", Mode: domain.PermissionDeny},
				{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			}},
			cap:  "filesystem.read",
			want: domain.PermissionAllow,
		},
		{
			name: "exact deny beats wildcard allow",
			policy: domain.PermissionPolicy{Rules: []domain.PermissionRule{
				{Capability: "*", Mode: domain.PermissionAllow},
				{Capability: "command.exec", Mode: domain.PermissionDeny},
			}},
			cap:  "command.exec",
			want: domain.PermissionDeny,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.Evaluate(tc.policy, tc.cap); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEvaluateLongestWildcardWins asserts the specific-before-general
// behavior: "filesystem.*" beats "*" when both match.
func TestEvaluateLongestWildcardWins(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "*", Mode: domain.PermissionAllow},
		{Capability: "filesystem.*", Mode: domain.PermissionConfirm},
	}}

	if got := e.Evaluate(policy, "filesystem.read"); got != domain.PermissionConfirm {
		t.Fatalf("long wildcard should win: got %s", got)
	}
	// A capability not under filesystem falls through to the global *.
	if got := e.Evaluate(policy, "network.outgoing"); got != domain.PermissionAllow {
		t.Fatalf("bare * should still match unrelated capability: got %s", got)
	}
}

// TestEvaluateWildcardBoundary asserts that a trailing .* doesn't swallow a
// sibling namespace ("file.*" must NOT match "files.read").
func TestEvaluateWildcardBoundary(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "file.*", Mode: domain.PermissionAllow},
	}}

	// "file.read" matches.
	if got := e.Evaluate(policy, "file.read"); got != domain.PermissionAllow {
		t.Fatalf("file.* should match file.read: got %s", got)
	}
	// "files.read" must NOT match — that would be a dotted-namespace confusion.
	if got := e.Evaluate(policy, "files.read"); got != domain.PermissionDeny {
		t.Fatalf("file.* must not match files.read (dotted namespace boundary): got %s", got)
	}
}

// TestEvaluateUnmatchedDenies asserts the deny-by-default stance.
func TestEvaluateUnmatchedDenies(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "filesystem.read", Mode: domain.PermissionAllow},
	}}
	if got := e.Evaluate(policy, "command.exec"); got != domain.PermissionDeny {
		t.Fatalf("unmatched cap should deny: got %s", got)
	}
	if got := e.Evaluate(policy, ""); got != domain.PermissionDeny {
		t.Fatalf("empty cap should deny: got %s", got)
	}
}

// TestEvaluateEmptyPolicy asserts that a policy with no rules denies
// everything.
func TestEvaluateEmptyPolicy(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{}
	if got := e.Evaluate(policy, "anything"); got != domain.PermissionDeny {
		t.Fatalf("empty policy must deny: got %s", got)
	}
}

// TestEvaluateContextPassThrough asserts EvaluateWithContext returns the same
// result as Evaluate today. Context is reserved for future attribution work
// (#20 foundation) and must not change current decisions.
func TestEvaluateContextPassThrough(t *testing.T) {
	e := NewEngine()
	policy := domain.PermissionPolicy{Rules: []domain.PermissionRule{
		{Capability: "filesystem.*", Mode: domain.PermissionConfirm},
	}}
	a := e.Evaluate(policy, "filesystem.write")
	b := e.EvaluateWithContext(policy, "filesystem.write", map[string]interface{}{"source": "telegram"})
	if a != b {
		t.Fatalf("EvaluateWithContext diverged from Evaluate: %s vs %s", a, b)
	}
}

// TestMatchWildcardInternals directly exercises the pattern matcher used by
// Evaluate. A separate test is useful because the pattern language is the
// spec the permission engine honors — if these semantics change we want a
// failure here before a consumer sees drift.
func TestMatchWildcardInternals(t *testing.T) {
	cases := []struct {
		pattern, cap string
		want         bool
	}{
		{"filesystem.read", "filesystem.read", true},
		{"filesystem.read", "filesystem.write", false},
		{"filesystem.*", "filesystem.read", true},
		{"filesystem.*", "filesystem.write.nested", true},
		{"filesystem.*", "filesystem", false}, // must have the dot separator
		{"filesystem.*", "filesystems.read", false},
		{"*", "anything", true},
		{"*", "", true},
		{"literal", "other", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.cap, func(t *testing.T) {
			if got := matchWildcard(tc.pattern, tc.cap); got != tc.want {
				t.Errorf("matchWildcard(%q, %q) = %v, want %v", tc.pattern, tc.cap, got, tc.want)
			}
		})
	}
}
