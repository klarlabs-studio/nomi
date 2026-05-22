package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/tools"
)

// testPlugin is a fully-featured plugin implementation for registry tests.
// Role participation is controlled by the embedded slices — a nil slice
// means the plugin claims that contribution in its manifest but returns
// no instances; to drop a role entirely, zero the corresponding manifest
// contribution list.
type testPlugin struct {
	id           string
	cardinality  ConnectionCardinality
	capabilities []string
	contributes  Contributions

	channels       []Channel
	tools          []tools.Tool
	triggers       []Trigger
	contextSources []ContextSource

	startCalls int
	stopCalls  int
	startErr   error
	stopErr    error
}

func (p *testPlugin) Manifest() PluginManifest {
	card := p.cardinality
	if card == "" {
		card = ConnectionSingle
	}
	return PluginManifest{
		ID:           p.id,
		Name:         "Test " + p.id,
		Version:      "1.0.0",
		Cardinality:  card,
		Capabilities: p.capabilities,
		Contributes:  p.contributes,
	}
}
func (p *testPlugin) Configure(context.Context, json.RawMessage) error { return nil }
func (p *testPlugin) Start(context.Context) error {
	p.startCalls++
	return p.startErr
}
func (p *testPlugin) Stop() error {
	p.stopCalls++
	return p.stopErr
}
func (p *testPlugin) Status() PluginStatus { return PluginStatus{Running: true, Ready: true} }

func (p *testPlugin) Channels() []Channel             { return p.channels }
func (p *testPlugin) Tools() []tools.Tool             { return p.tools }
func (p *testPlugin) Triggers() []Trigger             { return p.triggers }
func (p *testPlugin) ContextSources() []ContextSource { return p.contextSources }

// stubTool is a minimal tools.Tool for registry projection tests.
type stubTool struct{ name, cap string }

func (s stubTool) Name() string       { return s.name }
func (s stubTool) Capability() string { return s.cap }
func (s stubTool) Execute(context.Context, map[string]interface{}) (map[string]interface{}, error) {
	return nil, nil
}

type stubChannel struct{ connID, kind string }

func (s stubChannel) ConnectionID() string                             { return s.connID }
func (s stubChannel) Kind() string                                     { return s.kind }
func (s stubChannel) Send(context.Context, string, OutboundMessage) error { return nil }

type stubTrigger struct{ connID, kind string }

func (s stubTrigger) ConnectionID() string                        { return s.connID }
func (s stubTrigger) Kind() string                                { return s.kind }
func (s stubTrigger) Start(context.Context, TriggerCallback) error { return nil }
func (s stubTrigger) Stop() error                                 { return nil }

type stubContextSource struct{ connID, name string }

func (s stubContextSource) ConnectionID() string                      { return s.connID }
func (s stubContextSource) Name() string                              { return s.name }
func (s stubContextSource) Query(context.Context, ContextQueryRequest) (string, error) { return "", nil }

// TestList_ReturnsStableIDOrder pins the registry contract relied on
// by the UI: list-by-id ascending, deterministic across calls. Before
// this fix the UI's Plugins tab visibly shuffled cards on every 5s
// refetch because Go map iteration is randomized — a real UX bug
// surfaced during the live battle test.
func TestList_ReturnsStableIDOrder(t *testing.T) {
	r := NewRegistry()
	// Register in non-alphabetical order so any
	// "first-registered-wins" sort would also fail the assertion.
	for _, id := range []string{"com.nomi.zulu", "com.nomi.alpha", "com.nomi.mike"} {
		if err := r.Register(newSingleRolePlugin(id)); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	want := []string{"com.nomi.alpha", "com.nomi.mike", "com.nomi.zulu"}
	for i := 0; i < 20; i++ {
		got := r.List()
		gotIDs := make([]string, len(got))
		for j, p := range got {
			gotIDs[j] = p.Manifest().ID
		}
		for j := range want {
			if gotIDs[j] != want[j] {
				t.Fatalf("List() iter %d returned %v, want %v", i, gotIDs, want)
			}
		}
	}
}

func TestRegister_RequiresAtLeastOneContribution(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{
		id:           "com.nomi.empty",
		cardinality:  ConnectionSingle,
		capabilities: nil,
		contributes:  Contributions{},
	}
	err := r.Register(p)
	if err == nil || !strings.Contains(err.Error(), "contributes no roles") {
		t.Fatalf("expected no-roles error, got: %v", err)
	}
}

func TestRegister_RejectsDuplicateID(t *testing.T) {
	r := NewRegistry()
	p := newSingleRolePlugin("com.nomi.a")
	if err := r.Register(p); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Use a distinct but ID-matching instance to prove dedup is by ID.
	q := newSingleRolePlugin("com.nomi.a")
	err := r.Register(q)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected dup error, got: %v", err)
	}
}

func TestRegister_RejectsToolCapabilityNotInManifest(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{
		id:           "com.nomi.bad",
		cardinality:  ConnectionSingle,
		capabilities: []string{"foo.bar"},
		contributes: Contributions{
			Tools: []ToolContribution{{Name: "foo.baz", Capability: "foo.baz"}},
		},
	}
	err := r.Register(p)
	if err == nil || !strings.Contains(err.Error(), "not declared in manifest") {
		t.Fatalf("expected manifest-capability error, got: %v", err)
	}
}

func TestRegister_RejectsDuplicateCapabilityAcrossPlugins(t *testing.T) {
	r := NewRegistry()
	a := &testPlugin{
		id:           "com.nomi.a",
		cardinality:  ConnectionSingle,
		capabilities: []string{"shared.cap"},
		contributes: Contributions{
			Tools: []ToolContribution{{Name: "a.tool", Capability: "shared.cap"}},
		},
		tools: []tools.Tool{stubTool{name: "a.tool", cap: "shared.cap"}},
	}
	if err := r.Register(a); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	b := &testPlugin{
		id:           "com.nomi.b",
		cardinality:  ConnectionSingle,
		capabilities: []string{"shared.cap"},
		contributes: Contributions{
			Tools: []ToolContribution{{Name: "b.tool", Capability: "shared.cap"}},
		},
		tools: []tools.Tool{stubTool{name: "b.tool", cap: "shared.cap"}},
	}
	err := r.Register(b)
	if err == nil || !strings.Contains(err.Error(), "already provided") {
		t.Fatalf("expected capability-collision error, got: %v", err)
	}
	// The first plugin must still be registered; rejection of b
	// shouldn't have rolled back a.
	if _, err := r.Get("com.nomi.a"); err != nil {
		t.Fatalf("plugin a lost after failed register of b: %v", err)
	}
}

// Foundation capabilities (filesystem.*, network.outgoing, command.exec)
// are shared permission ceilings, not plugin-owned identities — multiple
// plugins must be able to claim them without colliding. Regression test for
// the GitHub + Obsidian + Browser triangle that otherwise fails to start.
func TestRegister_FoundationCapabilitiesAreShareable(t *testing.T) {
	r := NewRegistry()
	mk := func(id string) *testPlugin {
		return &testPlugin{
			id:           id,
			cardinality:  ConnectionSingle,
			capabilities: []string{"filesystem.write", "network.outgoing"},
			contributes: Contributions{
				Tools: []ToolContribution{
					{Name: id + ".write", Capability: "filesystem.write"},
					{Name: id + ".fetch", Capability: "network.outgoing"},
				},
			},
			tools: []tools.Tool{
				stubTool{name: id + ".write", cap: "filesystem.write"},
				stubTool{name: id + ".fetch", cap: "network.outgoing"},
			},
		}
	}
	for _, id := range []string{"com.nomi.github", "com.nomi.obsidian", "com.nomi.browser"} {
		if err := r.Register(mk(id)); err != nil {
			t.Fatalf("register %s should succeed for foundation caps: %v", id, err)
		}
	}
}

func TestRegister_MultiRolePlugin_FansOutToAllViews(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{
		id:           "com.nomi.everything",
		cardinality:  ConnectionMulti,
		capabilities: []string{"e.send"},
		contributes: Contributions{
			Channels:       []ChannelContribution{{Kind: "everything"}},
			Tools:          []ToolContribution{{Name: "e.send", Capability: "e.send"}},
			Triggers:       []TriggerContribution{{Kind: "every_watch", EventType: "e.tick"}},
			ContextSources: []ContextSourceContribution{{Name: "e.ctx"}},
		},
		channels:       []Channel{stubChannel{connID: "c1", kind: "everything"}},
		tools:          []tools.Tool{stubTool{name: "e.send", cap: "e.send"}},
		triggers:       []Trigger{stubTrigger{connID: "c1", kind: "every_watch"}},
		contextSources: []ContextSource{stubContextSource{connID: "c1", name: "e.ctx"}},
	}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := r.Channels(); len(got) != 1 || got[0].Kind() != "everything" {
		t.Fatalf("Channels projection: %+v", got)
	}
	if got := r.Tools(); len(got) != 1 || got[0].Name() != "e.send" {
		t.Fatalf("Tools projection: %+v", got)
	}
	if got := r.Triggers(); len(got) != 1 || got[0].Kind() != "every_watch" {
		t.Fatalf("Triggers projection: %+v", got)
	}
	if got := r.ContextSources(); len(got) != 1 || got[0].Name() != "e.ctx" {
		t.Fatalf("ContextSources projection: %+v", got)
	}
}

func TestRegister_SingleRolePlugin_OnlyShowsInOneView(t *testing.T) {
	r := NewRegistry()
	p := newSingleRolePlugin("com.nomi.channel_only")
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(r.Channels()) != 1 {
		t.Fatalf("Channels should have 1 entry, got %d", len(r.Channels()))
	}
	if len(r.Tools()) != 0 {
		t.Fatalf("Tools should be empty for channel-only plugin, got %d", len(r.Tools()))
	}
	if len(r.Triggers()) != 0 {
		t.Fatalf("Triggers should be empty for channel-only plugin, got %d", len(r.Triggers()))
	}
	if len(r.ContextSources()) != 0 {
		t.Fatalf("ContextSources should be empty, got %d", len(r.ContextSources()))
	}
}

func TestUnregister_RemovesFromAllViews(t *testing.T) {
	r := NewRegistry()
	p := newSingleRolePlugin("com.nomi.bye")
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Unregister("com.nomi.bye"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := r.Get("com.nomi.bye"); err == nil {
		t.Fatal("plugin should be gone from primary store")
	}
	if len(r.Channels()) != 0 {
		t.Fatal("plugin should be gone from Channels view")
	}
	// Capability ownership also cleared.
	if _, ok := r.CapabilityOwner("chan.send.com.nomi.bye"); ok {
		t.Fatal("capability owner should be cleared after unregister")
	}
}

func TestRegisterToolsInto_ProjectsIntoExistingToolsRegistry(t *testing.T) {
	r := NewRegistry()
	p := &testPlugin{
		id:           "com.nomi.toolplugin",
		cardinality:  ConnectionSingle,
		capabilities: []string{"t.x"},
		contributes: Contributions{
			Tools: []ToolContribution{{Name: "t.x", Capability: "t.x"}},
		},
		tools: []tools.Tool{stubTool{name: "t.x", cap: "t.x"}},
	}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	dst := tools.NewRegistry()
	if err := r.RegisterToolsInto(dst); err != nil {
		t.Fatalf("RegisterToolsInto: %v", err)
	}
	if got, err := dst.Get("t.x"); err != nil || got.Name() != "t.x" {
		t.Fatalf("tool not projected into destination registry: %v / %v", got, err)
	}
}

func TestStartAll_StopAll_CallEveryPlugin(t *testing.T) {
	r := NewRegistry()
	a := newSingleRolePlugin("com.nomi.a")
	b := newSingleRolePlugin("com.nomi.b")
	if err := r.Register(a); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := r.Register(b); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if err := r.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if a.startCalls != 1 || b.startCalls != 1 {
		t.Fatalf("Start not invoked on every plugin: a=%d b=%d", a.startCalls, b.startCalls)
	}
	if err := r.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if a.stopCalls != 1 || b.stopCalls != 1 {
		t.Fatalf("Stop not invoked on every plugin: a=%d b=%d", a.stopCalls, b.stopCalls)
	}
}

func TestStartAll_CollectsErrorsWithoutShortCircuit(t *testing.T) {
	r := NewRegistry()
	a := newSingleRolePlugin("com.nomi.a")
	a.startErr = errors.New("boom")
	b := newSingleRolePlugin("com.nomi.b")
	if err := r.Register(a); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := r.Register(b); err != nil {
		t.Fatalf("register b: %v", err)
	}
	err := r.StartAll(context.Background())
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	// b.Start should have been called even though a.Start failed.
	if b.startCalls != 1 {
		t.Fatalf("StartAll short-circuited on first error; b.startCalls=%d", b.startCalls)
	}
}

// newSingleRolePlugin builds a channel-only plugin with a single channel.
// Uses unique capability strings keyed off pluginID so multiple instances
// can co-exist in one registry without tripping the capability-collision
// guard in Register.
func newSingleRolePlugin(pluginID string) *testPlugin {
	cap := "chan.send." + pluginID
	return &testPlugin{
		id:           pluginID,
		cardinality:  ConnectionSingle,
		capabilities: []string{cap},
		contributes: Contributions{
			Channels: []ChannelContribution{{Kind: "test-" + pluginID}},
		},
		channels: []Channel{stubChannel{connID: "c-" + pluginID, kind: "test-" + pluginID}},
	}
}
