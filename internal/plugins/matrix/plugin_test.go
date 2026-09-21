package matrix

import (
	"testing"

	"go.klarlabs.de/nomi/internal/plugins"
)

func TestManifest(t *testing.T) {
	p := NewPlugin(nil, nil, nil, nil, nil, nil, nil, nil)
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("id: %s", m.ID)
	}
	if m.Cardinality != plugins.ConnectionMulti {
		t.Fatalf("cardinality: %v", m.Cardinality)
	}
	if len(m.Contributes.Channels) != 1 || m.Contributes.Channels[0].Kind != "matrix" {
		t.Fatalf("channels: %+v", m.Contributes.Channels)
	}
	if len(m.Contributes.Tools) != 1 || m.Contributes.Tools[0].Name != "matrix.post_message" {
		t.Fatalf("tools: %+v", m.Contributes.Tools)
	}
	foundToken := false
	for _, c := range m.Requires.Credentials {
		if c.Key == "access_token" && c.Required {
			foundToken = true
		}
	}
	if !foundToken {
		t.Fatal("access_token credential required")
	}
	hs, ok := m.Requires.ConfigSchema["homeserver_url"]
	if !ok || !hs.Required {
		t.Fatal("homeserver_url config required")
	}
	caps := map[string]bool{}
	for _, c := range m.Capabilities {
		caps[c] = true
	}
	if !caps["matrix.post"] || !caps["network.outgoing"] {
		t.Fatalf("capabilities: %v", m.Capabilities)
	}
}

func TestStatusBeforeStart(t *testing.T) {
	p := NewPlugin(nil, nil, nil, nil, nil, nil, nil, nil)
	st := p.Status()
	if st.Running {
		t.Fatal("should not be running before Start")
	}
}
