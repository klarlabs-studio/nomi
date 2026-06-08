package wasmplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
)

// loadEchoWASM reads the TinyGo-compiled echo plugin from the wasmhost
// testdata directory so the bytes aren't duplicated. go:embed can't
// reach across package boundaries, so we resolve the path at test
// time relative to this file's location.
func loadEchoWASM(t *testing.T) []byte {
	t.Helper()
	wd, _ := os.Getwd()
	// wd is internal/plugins/wasmplugin/; sibling dir holds the wasm.
	path := filepath.Join(wd, "..", "wasmhost", "testdata", "echo.wasm")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read echo.wasm: %v", err)
	}
	return body
}

// echoManifest is the host-side reflection of what the echo plugin's
// own plugin_manifest export returns. Held in Go so the adapter can be
// tested without rounding through the WASM-side manifest call.
func echoManifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:           "com.example.echo",
		Name:         "Echo",
		Version:      "0.0.1",
		Capabilities: []string{"echo.echo"},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{Name: "echo.echo", Capability: "echo.echo", Description: "Round-trip"},
			},
		},
	}
}

// TestPlugin_ToolsRoundTrip is the integration check the install
// pathway depends on: a WASM module loaded into wasmhost can be
// wrapped here, a Tool can be pulled out, and Execute returns the
// expected JSON.
func TestPlugin_ToolsRoundTrip(t *testing.T) {
	ctx := context.Background()
	loader := wasmhost.NewLoader(ctx)
	defer func() { _ = loader.Close(ctx) }()
	mod, err := loader.Load(ctx, "com.example.echo", loadEchoWASM(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := New(echoManifest(), mod, nil)
	defer func() { _ = p.Stop() }()

	if got := p.Manifest().ID; got != "com.example.echo" {
		t.Fatalf("manifest id = %q", got)
	}
	if !p.Status().Running {
		t.Fatal("freshly-constructed plugin should be running")
	}
	tools := p.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(tools))
	}
	out, err := tools[0].Execute(ctx, map[string]any{"hello": "world"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	echoed, _ := out["echoed"].(map[string]any)
	if echoed["hello"] != "world" {
		t.Fatalf("echo round-trip lost payload: %+v", out)
	}
}

func TestPlugin_StopClosesModule(t *testing.T) {
	ctx := context.Background()
	loader := wasmhost.NewLoader(ctx)
	defer func() { _ = loader.Close(ctx) }()
	mod, _ := loader.Load(ctx, "com.example.echo", loadEchoWASM(t))
	p := New(echoManifest(), mod, nil)
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.Status().Running {
		t.Fatal("Stop should clear Running")
	}
	// Subsequent Execute should fail because the module is closed.
	_, err := p.Tools()[0].Execute(ctx, map[string]any{})
	if err == nil {
		t.Fatal("Execute on stopped plugin should error")
	}
}

func TestPlugin_StopIsIdempotent(t *testing.T) {
	ctx := context.Background()
	loader := wasmhost.NewLoader(ctx)
	defer func() { _ = loader.Close(ctx) }()
	mod, _ := loader.Load(ctx, "com.example.echo", loadEchoWASM(t))
	p := New(echoManifest(), mod, nil)
	if err := p.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("second Stop should be nil, got %v", err)
	}
}
