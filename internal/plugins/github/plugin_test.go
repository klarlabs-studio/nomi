package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	gh "go.klarlabs.de/nomi/internal/integrations/github"
	"go.klarlabs.de/nomi/internal/secrets"
)

// memSecrets mirrors the in-memory secrets.Store used in other plugin
// tests (telegram). Lives here so this package can run without
// pulling in keyring or vault backends.
type memSecrets struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemSecrets() *memSecrets { return &memSecrets{data: map[string]string{}} }
func (m *memSecrets) Put(k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = v
	return nil
}
func (m *memSecrets) Get(k string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (m *memSecrets) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	return nil
}

func TestManifestShape(t *testing.T) {
	p := &Plugin{}
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("id: %s", m.ID)
	}
	if m.Cardinality != "multi" {
		t.Fatalf("cardinality: %s", m.Cardinality)
	}
	// Tool-only plugin — must not advertise a channel role.
	if len(m.Contributes.Channels) != 0 {
		t.Fatalf("github must not contribute a channel role: %+v", m.Contributes.Channels)
	}
	// Capabilities must include the four narrow caps the GitHub tools
	// will demand once they land. Asserted at scaffold time so a typo
	// surfaces before the dependent tasks (github-03..06) build on it.
	wantCaps := []string{"github.read", "github.write", "network.outgoing", "filesystem.write"}
	seen := map[string]bool{}
	for _, c := range m.Capabilities {
		seen[c] = true
	}
	for _, w := range wantCaps {
		if !seen[w] {
			t.Fatalf("manifest missing capability %q", w)
		}
	}
	// NetworkAllowlist must pin the GitHub host set so the runtime's
	// permission engine can intersect against the user's policy
	// without the plugin needing wildcard-network.
	wantHosts := []string{"api.github.com", "raw.githubusercontent.com", "codeload.github.com"}
	hosts := map[string]bool{}
	for _, h := range m.Requires.NetworkAllowlist {
		hosts[h] = true
	}
	for _, w := range wantHosts {
		if !hosts[w] {
			t.Fatalf("manifest missing network allowlist host %q", w)
		}
	}
	// Pin the canonical tool surface across all three families
	// (issues / pulls / repos). github-02-tools added pulls.create +
	// repos.search_code; both names are pinned here so a rename
	// surfaces in the manifest test rather than at integration time.
	wantTools := []string{
		"github.issues.list",
		"github.issues.get",
		"github.issues.create",
		"github.issues.comment",
		"github.pulls.list",
		"github.pulls.get",
		"github.pulls.comment",
		"github.pulls.review",
		"github.pulls.create",
		"github.repos.file_read",
		"github.repos.clone",
		"github.repos.search_code",
	}
	toolSeen := map[string]bool{}
	for _, tc := range m.Contributes.Tools {
		toolSeen[tc.Name] = true
	}
	for _, w := range wantTools {
		if !toolSeen[w] {
			t.Fatalf("manifest missing tool %q", w)
		}
	}
}

func TestLifecycle_StartStopStatus(t *testing.T) {
	p := NewPlugin(nil, nil, nil)
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

func TestConfigInt_CoerceShapes(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]any
		key     string
		want    int64
		wantErr bool
	}{
		{"missing key", map[string]any{}, "id", 0, true},
		{"nil value", map[string]any{"id": nil}, "id", 0, true},
		{"json float", map[string]any{"id": float64(42)}, "id", 42, false},
		{"explicit int64", map[string]any{"id": int64(99)}, "id", 99, false},
		{"plain int", map[string]any{"id": 7}, "id", 7, false},
		{"numeric string", map[string]any{"id": "1234"}, "id", 1234, false},
		{"empty string", map[string]any{"id": ""}, "id", 0, true},
		{"non-numeric string", map[string]any{"id": "abc"}, "id", 0, true},
		{"bool", map[string]any{"id": true}, "id", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configInt(tc.config, tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAuthClientFor_OverrideIsRespected(t *testing.T) {
	p := NewPlugin(nil, nil, nil)
	called := false
	override := gh.NewAuthClient(nil) // sentinel pointer, never used
	p.SetAuthOverride(func(_ *domain.Connection) (*gh.AuthClient, error) {
		called = true
		return override, nil
	})
	conn := &domain.Connection{ID: "c1", PluginID: PluginID}
	got, err := p.authClientFor(conn)
	if err != nil {
		t.Fatalf("authClientFor: %v", err)
	}
	if !called {
		t.Fatal("override factory not invoked")
	}
	if got != override {
		t.Fatal("override return value not propagated")
	}
}

func TestAuthClientFor_LoadsFromSecrets(t *testing.T) {
	// Generate a fresh RSA key, PEM-encode, store in an in-memory
	// secrets.Store, and verify authClientFor pulls + parses it.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	store := newMemSecrets()
	const secretKey = "test/private-key"
	if err := store.Put(secretKey, string(pemBytes)); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	ref := secrets.NewReference(secretKey)

	p := NewPlugin(nil, nil, store)
	conn := &domain.Connection{
		ID:       "c1",
		PluginID: PluginID,
		Config: map[string]any{
			configAppID:          float64(42), // JSON-decoded shape
			configInstallationID: float64(555),
		},
		CredentialRefs: map[string]string{
			credentialPrivateKey: ref,
		},
	}
	client, err := p.authClientFor(conn)
	if err != nil {
		t.Fatalf("authClientFor: %v", err)
	}
	if client == nil {
		t.Fatal("nil auth client")
	}
	// Cache should return the same pointer on second call.
	client2, err := p.authClientFor(conn)
	if err != nil {
		t.Fatalf("authClientFor (cached): %v", err)
	}
	if client != client2 {
		t.Fatal("expected cached AuthClient pointer reuse across calls")
	}
}

func TestAuthClientFor_RejectsMissingCredential(t *testing.T) {
	store := newMemSecrets()
	p := NewPlugin(nil, nil, store)
	conn := &domain.Connection{
		ID:       "c1",
		PluginID: PluginID,
		Config: map[string]any{
			configAppID:          float64(42),
			configInstallationID: float64(555),
		},
		// No CredentialRefs at all — missing private key.
	}
	if _, err := p.authClientFor(conn); err == nil {
		t.Fatal("expected error when private key credential missing")
	}
}
