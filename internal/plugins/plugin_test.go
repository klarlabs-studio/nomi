package plugins

import (
	"context"
	"encoding/json"
	"testing"

	"go.klarlabs.de/nomi/internal/tools"
)

// This file tests the plugin contract types only. Registry behavior,
// role-view projection, and concrete plugin implementations are covered
// in their own packages.

func TestManifestRoundTripsThroughJSON(t *testing.T) {
	orig := PluginManifest{
		ID:           "com.nomi.test",
		Name:         "Test Plugin",
		Version:      "1.2.3",
		Author:       "Nomi",
		Description:  "A test fixture",
		Cardinality:  ConnectionMulti,
		Capabilities: []string{"test.send", "network.outgoing"},
		Contributes: Contributions{
			Channels: []ChannelContribution{{
				Kind:              "test",
				Description:       "channel desc",
				SupportsThreading: true,
			}},
			Tools: []ToolContribution{{
				Name:               "test.send",
				Capability:         "test.send",
				Description:        "tool desc",
				RequiresConnection: true,
			}},
			Triggers: []TriggerContribution{{
				Kind:        "test_watch",
				EventType:   "test.event",
				Description: "trigger desc",
			}},
			ContextSources: []ContextSourceContribution{{
				Name:        "test.context",
				Description: "context desc",
			}},
		},
		Requires: Requirements{
			Credentials: []CredentialSpec{{
				Kind:     "bot_token",
				Key:      "bot_token",
				Label:    "Bot Token",
				Required: true,
			}},
			ConfigSchema: map[string]ConfigField{
				"enabled": {Type: "boolean", Label: "Enabled", Default: "false"},
			},
		},
	}

	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PluginManifest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != orig.ID || got.Version != orig.Version {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, orig)
	}
	if got.Cardinality != ConnectionMulti {
		t.Fatalf("cardinality: %s", got.Cardinality)
	}
	if len(got.Contributes.Channels) != 1 || got.Contributes.Channels[0].Kind != "test" {
		t.Fatalf("channels lost in round-trip: %+v", got.Contributes.Channels)
	}
	if !got.Contributes.Channels[0].SupportsThreading {
		t.Fatalf("SupportsThreading lost")
	}
	if len(got.Contributes.Tools) != 1 || !got.Contributes.Tools[0].RequiresConnection {
		t.Fatalf("tools lost in round-trip: %+v", got.Contributes.Tools)
	}
	if len(got.Contributes.Triggers) != 1 || got.Contributes.Triggers[0].EventType != "test.event" {
		t.Fatalf("triggers lost in round-trip: %+v", got.Contributes.Triggers)
	}
	if len(got.Contributes.ContextSources) != 1 {
		t.Fatalf("context sources lost in round-trip")
	}
	if len(got.Requires.Credentials) != 1 || got.Requires.Credentials[0].Kind != "bot_token" {
		t.Fatalf("credentials lost in round-trip: %+v", got.Requires)
	}
}

func TestContributionsHasRole(t *testing.T) {
	empty := Contributions{}
	for _, role := range []string{"channel", "tool", "trigger", "context_source"} {
		if empty.HasRole(role) {
			t.Fatalf("empty contributions should not claim role %q", role)
		}
	}
	if empty.HasRole("unknown") {
		t.Fatal("HasRole should return false for unknown roles")
	}

	full := Contributions{
		Channels:       []ChannelContribution{{Kind: "x"}},
		Tools:          []ToolContribution{{Name: "x.y", Capability: "x.y"}},
		Triggers:       []TriggerContribution{{Kind: "x_watch", EventType: "x.evt"}},
		ContextSources: []ContextSourceContribution{{Name: "x.ctx"}},
	}
	for _, role := range []string{"channel", "tool", "trigger", "context_source"} {
		if !full.HasRole(role) {
			t.Fatalf("full contributions should claim role %q", role)
		}
	}
}

func TestCardinalityConstantsAreDistinct(t *testing.T) {
	// Not redundant: guards against a future refactor that collapses the
	// values into shared constants. Downstream code uses string equality
	// against these so any collision would silently misclassify plugins.
	if ConnectionSingle == ConnectionMulti || ConnectionMulti == ConnectionMultiMulti || ConnectionSingle == ConnectionMultiMulti {
		t.Fatal("cardinality constants must be pairwise distinct")
	}
}

// --- Interface satisfaction tests ---
//
// These tests verify a single struct can simultaneously satisfy every
// role interface (a plugin that plays all four roles). If this ever
// stops compiling, the role interfaces have grown method signatures
// that conflict with each other — a signal to split them.

type fakePlugin struct{}

func (fakePlugin) Manifest() PluginManifest                         { return PluginManifest{ID: "com.nomi.fake"} }
func (fakePlugin) Configure(context.Context, json.RawMessage) error { return nil }
func (fakePlugin) Start(context.Context) error                      { return nil }
func (fakePlugin) Stop() error                                      { return nil }
func (fakePlugin) Status() PluginStatus                             { return PluginStatus{Running: true, Ready: true} }

func (fakePlugin) Channels() []Channel             { return nil }
func (fakePlugin) Tools() []tools.Tool             { return nil }
func (fakePlugin) Triggers() []Trigger             { return nil }
func (fakePlugin) ContextSources() []ContextSource { return nil }

func TestFakePluginSatisfiesAllRoles(t *testing.T) {
	var p Plugin = fakePlugin{}
	if _, ok := p.(ChannelProvider); !ok {
		t.Fatal("fakePlugin should satisfy ChannelProvider")
	}
	if _, ok := p.(ToolProvider); !ok {
		t.Fatal("fakePlugin should satisfy ToolProvider")
	}
	if _, ok := p.(TriggerProvider); !ok {
		t.Fatal("fakePlugin should satisfy TriggerProvider")
	}
	if _, ok := p.(ContextSourceProvider); !ok {
		t.Fatal("fakePlugin should satisfy ContextSourceProvider")
	}
}

func TestFakeChannelKindAndConnectionID(t *testing.T) {
	ch := fakeChannel{connID: "conn-1", kind: "test"}
	if ch.ConnectionID() != "conn-1" {
		t.Fatalf("ConnectionID: %s", ch.ConnectionID())
	}
	if ch.Kind() != "test" {
		t.Fatalf("Kind: %s", ch.Kind())
	}
	if err := ch.Send(context.Background(), "ext-1", OutboundMessage{Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

type fakeChannel struct {
	connID string
	kind   string
}

func (c fakeChannel) ConnectionID() string                                      { return c.connID }
func (c fakeChannel) Kind() string                                              { return c.kind }
func (c fakeChannel) Send(_ context.Context, _ string, _ OutboundMessage) error { return nil }

func TestTriggerEventShape(t *testing.T) {
	// This test pins the TriggerEvent field names; renaming any of them
	// is a breaking change for every trigger implementation.
	evt := TriggerEvent{
		ConnectionID: "conn-1",
		Kind:         "inbox_watch",
		Goal:         "New email from bob@example.com: hi",
		Metadata:     map[string]interface{}{"from": "bob@example.com"},
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"ConnectionID", "Kind", "Goal", "Metadata"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("TriggerEvent missing expected field %q in JSON", key)
		}
	}
}
