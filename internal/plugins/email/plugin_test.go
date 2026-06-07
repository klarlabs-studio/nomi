package email

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestManifestShape(t *testing.T) {
	p := &Plugin{}
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("id: %s", m.ID)
	}
	if m.Cardinality != "multi" {
		t.Fatalf("cardinality: %s", m.Cardinality)
	}
	if len(m.Contributes.Channels) != 1 || m.Contributes.Channels[0].Kind != "email" {
		t.Fatalf("channel contribution: %+v", m.Contributes.Channels)
	}
	if !m.Contributes.Channels[0].SupportsThreading {
		t.Fatal("email channel must advertise SupportsThreading=true")
	}
	// Must declare network + filesystem.read for the manifest-intersection
	// ceiling at tool-exec time.
	want := map[string]bool{"email.send": true, "network.outgoing": true, "filesystem.read": true}
	if len(m.Capabilities) != len(want) {
		t.Fatalf("capabilities: %v", m.Capabilities)
	}
	for _, cap := range m.Capabilities {
		if !want[cap] {
			t.Fatalf("unexpected capability %s", cap)
		}
	}
	// Must require the password credential so UI validates inputs correctly.
	if len(m.Requires.Credentials) != 1 || m.Requires.Credentials[0].Key != "password" {
		t.Fatalf("credentials: %+v", m.Requires.Credentials)
	}
	// Must expose per-connection config fields for the Plugins-tab form.
	for _, field := range []string{"imap_host", "imap_port", "smtp_host", "smtp_port", "username"} {
		if _, ok := m.Requires.ConfigSchema[field]; !ok {
			t.Fatalf("missing config field %q", field)
		}
	}
}

func TestExtractRecipientAndBody(t *testing.T) {
	addr, body := extractRecipientAndBody("To: bob@example.com\nHello Bob")
	if addr != "bob@example.com" {
		t.Fatalf("addr: %q", addr)
	}
	if body != "Hello Bob" {
		t.Fatalf("body: %q", body)
	}

	// Missing prefix — caller gets a clear empty-string signal instead of
	// silently dispatching to an unknown recipient.
	addr, body = extractRecipientAndBody("Just a body.")
	if addr != "" {
		t.Fatalf("should return empty addr when prefix absent, got %q", addr)
	}
	if body != "Just a body." {
		t.Fatalf("body should pass through: %q", body)
	}
}

func TestIntFromConfig_DefaultsWhenAbsent(t *testing.T) {
	cfg := map[string]interface{}{}
	if got := intFromConfig(cfg, "missing", 42); got != 42 {
		t.Fatalf("default: %d", got)
	}
	// JSON unmarshal gives numbers as float64.
	cfg["imap_port"] = float64(993)
	if got := intFromConfig(cfg, "imap_port", 0); got != 993 {
		t.Fatalf("float64 unmarshal: %d", got)
	}
	// String fallback for values arriving from form UIs that send strings.
	cfg["smtp_port"] = "587"
	if got := intFromConfig(cfg, "smtp_port", 0); got != 587 {
		t.Fatalf("string fallback: %d", got)
	}
}

func TestFirstContactPolicy_DefaultsToDropOnMissing(t *testing.T) {
	p := &Plugin{}
	// No connection registered → falls through to Drop (safe default).
	got := p.firstContactPolicy("nonexistent")
	if got != domain.FirstContactDrop {
		t.Fatalf("expected drop default, got %s", got)
	}
}
