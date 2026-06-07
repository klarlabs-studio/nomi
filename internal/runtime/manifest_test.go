package runtime

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/permissions"
)

func TestEffectivePermissionMode_DesktopRun(t *testing.T) {
	// Runs with nil Source fall through to the assistant policy unchanged.
	rt := &Runtime{permEngine: permissions.NewEngine()}
	assistant := &domain.AssistantDefinition{
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "command.exec", Mode: domain.PermissionAllow},
			},
		},
	}
	run := &domain.Run{ID: "r1"} // Source nil
	got := rt.effectivePermissionMode(run, assistant, "command.exec")
	if got != domain.PermissionAllow {
		t.Fatalf("desktop run should see assistant policy directly; got %s", got)
	}
}

func TestEffectivePermissionMode_UnknownConnectorDenies(t *testing.T) {
	rt := &Runtime{
		permEngine: permissions.NewEngine(),
		connectorManifest: func(name string) ([]string, bool) {
			return nil, false
		},
	}
	assistant := &domain.AssistantDefinition{
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "filesystem.read", Mode: domain.PermissionAllow},
			},
		},
	}
	source := "unknown-connector"
	run := &domain.Run{ID: "r1", Source: &source}
	if got := rt.effectivePermissionMode(run, assistant, "filesystem.read"); got != domain.PermissionDeny {
		t.Fatalf("unknown connector source must be denied; got %s", got)
	}
}

func TestEffectivePermissionMode_NoLookupDenies(t *testing.T) {
	// connectorManifest == nil: we have a source but no way to resolve it.
	// Secure default is deny.
	rt := &Runtime{permEngine: permissions.NewEngine()}
	assistant := &domain.AssistantDefinition{
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{{Capability: "*", Mode: domain.PermissionAllow}},
		},
	}
	src := "telegram"
	run := &domain.Run{ID: "r1", Source: &src}
	if got := rt.effectivePermissionMode(run, assistant, "command.exec"); got != domain.PermissionDeny {
		t.Fatalf("missing manifest lookup must deny; got %s", got)
	}
}

func TestEffectivePermissionMode_CapabilityNotInManifestDenies(t *testing.T) {
	rt := &Runtime{
		permEngine: permissions.NewEngine(),
		connectorManifest: func(name string) ([]string, bool) {
			// Connector only declares network.outgoing.
			return []string{"network.outgoing"}, true
		},
	}
	assistant := &domain.AssistantDefinition{
		// Assistant would allow command.exec on its own.
		PermissionPolicy: domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "command.exec", Mode: domain.PermissionAllow},
			},
		},
	}
	src := "telegram"
	run := &domain.Run{ID: "r1", Source: &src}
	got := rt.effectivePermissionMode(run, assistant, "command.exec")
	if got != domain.PermissionDeny {
		t.Fatalf("capability outside manifest must be denied regardless of assistant policy; got %s", got)
	}
}

func TestEffectivePermissionMode_IntersectsToStrictest(t *testing.T) {
	rt := &Runtime{
		permEngine: permissions.NewEngine(),
		connectorManifest: func(name string) ([]string, bool) {
			return []string{"filesystem.*", "network.outgoing"}, true
		},
	}
	src := "telegram"
	run := &domain.Run{ID: "r1", Source: &src}

	cases := []struct {
		name          string
		assistantMode domain.PermissionMode
		capability    string
		wantEffective domain.PermissionMode
	}{
		{"allow+allow → allow", domain.PermissionAllow, "filesystem.read", domain.PermissionAllow},
		{"confirm+allow → confirm", domain.PermissionConfirm, "filesystem.read", domain.PermissionConfirm},
		{"deny+allow → deny", domain.PermissionDeny, "filesystem.read", domain.PermissionDeny},
		{"outside manifest → deny", domain.PermissionAllow, "command.exec", domain.PermissionDeny},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assistant := &domain.AssistantDefinition{
				PermissionPolicy: domain.PermissionPolicy{
					Rules: []domain.PermissionRule{
						{Capability: tc.capability, Mode: tc.assistantMode},
					},
				},
			}
			got := rt.effectivePermissionMode(run, assistant, tc.capability)
			if got != tc.wantEffective {
				t.Fatalf("got %s, want %s", got, tc.wantEffective)
			}
		})
	}
}

func TestIntersectModes(t *testing.T) {
	allow := domain.PermissionAllow
	confirm := domain.PermissionConfirm
	deny := domain.PermissionDeny

	cases := []struct {
		a, b, want domain.PermissionMode
	}{
		{allow, allow, allow},
		{allow, confirm, confirm},
		{confirm, allow, confirm},
		{allow, deny, deny},
		{deny, allow, deny},
		{confirm, deny, deny},
		{deny, confirm, deny},
		{deny, deny, deny},
		{confirm, confirm, confirm},
	}
	for _, tc := range cases {
		if got := intersectModes(tc.a, tc.b); got != tc.want {
			t.Errorf("intersectModes(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
	}
}
