package mnemos

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeSecrets implements just enough of secrets.Store to satisfy
// Configure paths that resolve a token reference. Returns the key
// itself as the value so tests can verify the reference was followed.
type fakeSecrets struct {
	values map[string]string
}

func (f *fakeSecrets) Get(key string) (string, error) {
	if v, ok := f.values[key]; ok {
		return v, nil
	}
	return "", nil
}
func (f *fakeSecrets) Put(key, value string) error {
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}
func (f *fakeSecrets) Delete(key string) error { delete(f.values, key); return nil }

func TestManifest_ContractRequirements(t *testing.T) {
	m := buildManifest()
	if m.ID != PluginID {
		t.Errorf("ID = %q, want %q", m.ID, PluginID)
	}
	if m.Version == "" {
		t.Error("Version must be set")
	}
	if len(m.Capabilities) != 2 {
		t.Errorf("Capabilities count = %d, want 2", len(m.Capabilities))
	}
	if !m.Contributes.HasRole("tool") {
		t.Error("plugin must declare tool role")
	}
	if !m.Contributes.HasRole("context_source") {
		t.Error("plugin must declare context_source role")
	}
	if m.Contributes.HasRole("channel") {
		t.Error("plugin must NOT declare channel role")
	}
	if m.Contributes.HasRole("trigger") {
		t.Error("plugin must NOT declare trigger role")
	}
	// Every ToolContribution's Capability must be in the manifest's
	// declared Capabilities — registry enforces this at register-time.
	caps := map[string]bool{}
	for _, c := range m.Capabilities {
		caps[c] = true
	}
	for _, tc := range m.Contributes.Tools {
		if !caps[tc.Capability] {
			t.Errorf("tool %q references undeclared capability %q", tc.Name, tc.Capability)
		}
	}
}

func TestPlugin_Lifecycle(t *testing.T) {
	p := New(&fakeSecrets{})

	// Pre-Configure status: not running, not ready.
	if s := p.Status(); s.Running || s.Ready {
		t.Errorf("pre-Start status = %+v, want stopped", s)
	}

	// Configure with one connection. token_ref omitted; reads-only mode.
	cfg, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{
			{ID: "c1", BaseURL: "http://localhost:9090", VisibilityDefault: "personal"},
		},
	})
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// Start → running.
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := p.Status(); !s.Running || !s.Ready {
		t.Errorf("post-Start status = %+v, want running+ready", s)
	}

	// Resolving the configured connection works; an unknown one is a
	// not-found error.
	if _, err := p.resolveConnection("c1"); err != nil {
		t.Errorf("resolveConnection(c1): %v", err)
	}
	if _, err := p.resolveConnection("nope"); err == nil {
		t.Error("resolveConnection on unknown id should fail")
	}

	// Stop → not running.
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if s := p.Status(); s.Running {
		t.Errorf("post-Stop status = %+v, want stopped", s)
	}
}

func TestPlugin_Configure_RejectsConnectionWithoutBaseURL(t *testing.T) {
	p := New(&fakeSecrets{})
	cfg, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{
			{ID: "c1"}, // missing BaseURL
		},
	})
	if err := p.Configure(context.Background(), cfg); err == nil {
		t.Error("expected validation error")
	}
}

func TestPlugin_Configure_HotReload(t *testing.T) {
	p := New(&fakeSecrets{})

	// First config: one connection.
	cfg1, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{{ID: "c1", BaseURL: "http://a"}},
	})
	if err := p.Configure(context.Background(), cfg1); err != nil {
		t.Fatal(err)
	}

	// Second config: replace with a different connection.
	cfg2, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{{ID: "c2", BaseURL: "http://b"}},
	})
	if err := p.Configure(context.Background(), cfg2); err != nil {
		t.Fatal(err)
	}

	if _, err := p.resolveConnection("c1"); err == nil {
		t.Error("hot-reload should drop the old connection")
	}
	if _, err := p.resolveConnection("c2"); err != nil {
		t.Errorf("hot-reload should add the new connection: %v", err)
	}
}

func TestPlugin_Tools_ReturnsAllSix(t *testing.T) {
	p := New(&fakeSecrets{})
	tools := p.Tools()
	if got := len(tools); got != 6 {
		t.Fatalf("Tools() = %d, want 6", got)
	}
	got := make(map[string]string, len(tools))
	for _, tl := range tools {
		got[tl.Name()] = tl.Capability()
	}
	wantCaps := map[string]string{
		ToolEventsAppend:      CapWrite,
		ToolClaimsAppend:      CapWrite,
		ToolClaimsList:        CapRead,
		ToolRelationshipsList: CapRead,
		ToolEmbeddingsAppend:  CapWrite,
		ToolSearch:            CapRead,
	}
	for name, wantCap := range wantCaps {
		gotCap, ok := got[name]
		if !ok {
			t.Errorf("missing tool %q", name)
			continue
		}
		if gotCap != wantCap {
			t.Errorf("tool %q capability = %q, want %q", name, gotCap, wantCap)
		}
	}
}

func TestPlugin_ContextSources_OnePerConnection(t *testing.T) {
	p := New(&fakeSecrets{})
	cfg, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{
			{ID: "personal", BaseURL: "http://a"},
			{ID: "company", BaseURL: "http://b"},
		},
	})
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	sources := p.ContextSources()
	if len(sources) != 2 {
		t.Errorf("ContextSources() = %d, want 2", len(sources))
	}
	for _, s := range sources {
		if s.Name() != ContextSourceName {
			t.Errorf("source name = %q, want %q", s.Name(), ContextSourceName)
		}
	}
}

func TestPlugin_ToolValidation_RejectsMissingConnection(t *testing.T) {
	p := New(&fakeSecrets{})
	// No Configure call — connection map is empty.
	tools := p.Tools()
	// Each tool should fail with the same not-found shape when
	// connection_id is omitted or unknown.
	for _, tl := range tools {
		_, err := tl.Execute(context.Background(), map[string]interface{}{})
		if err == nil {
			t.Errorf("tool %q: missing connection_id should error", tl.Name())
		}
	}
}
