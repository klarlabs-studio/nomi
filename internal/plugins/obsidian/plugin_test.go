package obsidian

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

func TestManifestShape(t *testing.T) {
	p := &Plugin{}
	m := p.Manifest()

	if m.ID != PluginID {
		t.Fatalf("id: got %q, want %q", m.ID, PluginID)
	}
	if m.Cardinality != "multi" {
		t.Fatalf("cardinality: got %q, want multi", m.Cardinality)
	}

	wantCaps := map[string]bool{
		"filesystem.read":  true,
		"filesystem.write": true,
	}
	got := map[string]bool{}
	for _, c := range m.Capabilities {
		got[c] = true
	}
	for w := range wantCaps {
		if !got[w] {
			t.Fatalf("manifest missing capability %q", w)
		}
	}
	// Security story: no network access at all. Asserted at scaffold
	// time so a future tool that needs network surfaces this constraint
	// loudly rather than quietly extending the capability set.
	for _, forbidden := range []string{"network.outgoing", "network.incoming"} {
		if got[forbidden] {
			t.Fatalf("obsidian must not advertise %q capability", forbidden)
		}
	}
	if len(m.Requires.NetworkAllowlist) != 0 {
		t.Fatalf("network allowlist must be empty, got %v", m.Requires.NetworkAllowlist)
	}

	// No credentials — the differentiator vs cloud notes services.
	if len(m.Requires.Credentials) != 0 {
		t.Fatalf("obsidian must not require credentials, got %v", m.Requires.Credentials)
	}

	// Vault path config field must be required so the UI prompts for it.
	field, ok := m.Requires.ConfigSchema[configVaultPath]
	if !ok {
		t.Fatalf("manifest missing config field %q", configVaultPath)
	}
	if !field.Required {
		t.Fatal("vault_path must be required")
	}

	// Tool surface (obsidian-02). Pin the names so a future rename
	// surfaces in the manifest test rather than at integration time.
	wantTools := []string{
		"obsidian.read_note",
		"obsidian.create_note",
		"obsidian.update_note",
		"obsidian.search_notes",
		"obsidian.link_notes",
	}
	toolSeen := map[string]plugins.ToolContribution{}
	for _, tc := range m.Contributes.Tools {
		toolSeen[tc.Name] = tc
	}
	for _, w := range wantTools {
		tc, ok := toolSeen[w]
		if !ok {
			t.Fatalf("manifest missing tool %q", w)
		}
		if !tc.RequiresConnection {
			t.Fatalf("tool %q must require a connection", w)
		}
		if tc.Capability != "filesystem.read" && tc.Capability != "filesystem.write" {
			t.Fatalf("tool %q has unexpected capability %q", w, tc.Capability)
		}
	}

	// Context-source surface (obsidian-03).
	if len(m.Contributes.ContextSources) != 1 {
		t.Fatalf("expected 1 context source contribution, got %d", len(m.Contributes.ContextSources))
	}
	if cs := m.Contributes.ContextSources[0]; cs.Name != "obsidian.vault" {
		t.Fatalf("context source name: got %q, want obsidian.vault", cs.Name)
	}
	if len(m.Contributes.Channels) != 0 {
		t.Fatalf("obsidian must not contribute channels: %+v", m.Contributes.Channels)
	}
}

func TestLifecycle_StartStopStatus(t *testing.T) {
	p := NewPlugin(nil, nil)
	if p.Status().Running {
		t.Fatal("plugin should not start running")
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !p.Status().Running {
		t.Fatal("status should report running after Start")
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if p.Status().Running {
		t.Fatal("status should not report running after Stop")
	}
}

func TestVaultPathForConnection(t *testing.T) {
	cases := []struct {
		name    string
		conn    *domain.Connection
		want    string
		wantErr error
	}{
		{
			name:    "nil connection",
			conn:    nil,
			wantErr: ErrConnectionRequired,
		},
		{
			name:    "missing config",
			conn:    &domain.Connection{Config: map[string]any{}},
			wantErr: ErrVaultPathMissing,
		},
		{
			name:    "empty string",
			conn:    &domain.Connection{Config: map[string]any{configVaultPath: ""}},
			wantErr: ErrVaultPathMissing,
		},
		{
			name:    "wrong type",
			conn:    &domain.Connection{Config: map[string]any{configVaultPath: 42}},
			wantErr: ErrVaultPathInvalidType,
		},
		{
			name: "valid path",
			conn: &domain.Connection{Config: map[string]any{configVaultPath: "/tmp/vault"}},
			want: "/tmp/vault",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VaultPathForConnection(tc.conn)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got err %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
