package scout

import (
	"testing"

	"go.klarlabs.de/nomi/internal/plugins"
)

func TestManifestSurface(t *testing.T) {
	p := &Plugin{}
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("ID: %q", m.ID)
	}
	if m.Cardinality != plugins.ConnectionMulti {
		t.Fatal("expected ConnectionMulti cardinality")
	}
	if len(m.Contributes.Tools) != 6 {
		t.Fatalf("expected 6 tool contributions, got %d", len(m.Contributes.Tools))
	}
	wantTools := map[string]bool{
		"scout.navigate":   true,
		"scout.observe":    true,
		"scout.click":      true,
		"scout.type":       true,
		"scout.screenshot": true,
		"scout.extract":    true,
	}
	for _, tc := range m.Contributes.Tools {
		if !wantTools[tc.Name] {
			t.Errorf("unexpected tool: %s", tc.Name)
		}
		if tc.Capability != "scout.browse" {
			t.Errorf("tool %s has wrong capability: %s", tc.Name, tc.Capability)
		}
		if !tc.RequiresConnection {
			t.Errorf("tool %s should require connection_id", tc.Name)
		}
	}
}

func TestToolsReturnsExpectedSet(t *testing.T) {
	p := &Plugin{}
	ts := p.Tools()
	if len(ts) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(ts))
	}
	for _, tool := range ts {
		if tool.Capability() != "scout.browse" {
			t.Errorf("tool %s capability=%s, want scout.browse", tool.Name(), tool.Capability())
		}
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"mcp", []string{"mcp"}},
		{"mcp,--port,7777", []string{"mcp", "--port", "7777"}},
		{"mcp --port 7777", []string{"mcp", "--port", "7777"}},
		{"  spaced  ,  out  ", []string{"spaced", "out"}},
	}
	for _, c := range cases {
		got := splitArgs(c.in)
		if !equalSlices(got, c.want) {
			t.Errorf("splitArgs(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStatusBeforeStart(t *testing.T) {
	p := &Plugin{}
	s := p.Status()
	if s.Running {
		t.Fatal("expected !Running before Start")
	}
}
