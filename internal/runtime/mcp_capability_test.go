package runtime

import (
	"context"
	"testing"

	"go.klarlabs.de/nomi/internal/tools"
)

type stubCapTool struct {
	name string
	cap  string
}

func (t *stubCapTool) Name() string       { return t.name }
func (t *stubCapTool) Capability() string { return t.cap }
func (t *stubCapTool) Execute(context.Context, map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func TestMCPToolSlug(t *testing.T) {
	cases := map[string]string{
		"write_file":  "write_file",
		"Write File":  "write_file",
		"search-docs": "search_docs",
		"":            "",
	}
	for in, want := range cases {
		if got := mcpToolSlug(in); got != want {
			t.Fatalf("mcpToolSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefineMCPCapabilityUsesDiscoveredTool(t *testing.T) {
	reg := tools.NewRegistry()
	for _, name := range []string{"mcp.docs.search", "mcp.docs.write_file"} {
		if err := reg.Register(&stubCapTool{name: name, cap: name}); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runtime{toolExecutor: tools.NewExecutor(reg)}

	got := r.refineMCPCapability("mcp.call", "mcp.tools", map[string]interface{}{
		"tool": "write_file",
	})
	if got != "mcp.docs.write_file" {
		t.Fatalf("got %q, want mcp.docs.write_file", got)
	}

	got = r.refineMCPCapability("mcp.call", "mcp.tools", map[string]interface{}{
		"tool": "search",
	})
	if got != "mcp.docs.search" {
		t.Fatalf("got %q, want mcp.docs.search", got)
	}

	got = r.refineMCPCapability("mcp.call", "mcp.tools", map[string]interface{}{
		"tool": "nope",
	})
	if got != "mcp.tools" {
		t.Fatalf("got %q, want mcp.tools", got)
	}

	got = r.refineMCPCapability("filesystem.read", "filesystem.read", nil)
	if got != "filesystem.read" {
		t.Fatalf("got %q", got)
	}
}
