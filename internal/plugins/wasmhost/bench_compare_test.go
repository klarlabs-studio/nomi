package wasmhost

import (
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// loadForBench is the spike-only loader that handles both TinyGo's WASI
// command modules (main = _start, must be skipped so the module stays
// alive) and stdgo's WASI reactor modules (must call _initialize once
// to bring the runtime up before exports work). Production loader picks
// one strategy after we decide whether to support stdgo at all.
func loadForBench(ctx context.Context, l *Loader, pluginID string, wasm []byte, reactor bool) (*Module, error) {
	if err := registerHostImports(ctx, l.runtime); err != nil {
		return nil, err
	}
	cfg := wazero.NewModuleConfig().WithStartFunctions()
	compiled, err := l.runtime.CompileModule(ctx, wasm)
	if err != nil {
		return nil, err
	}
	mod, err := l.runtime.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, err
	}
	if reactor {
		init := mod.ExportedFunction("_initialize")
		if init == nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("reactor module %s missing _initialize", pluginID)
		}
		if _, err := init.Call(ctx); err != nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("_initialize: %w", err)
		}
	}
	m := &Module{runtime: l.runtime, module: mod, pluginID: pluginID}
	for _, fn := range []struct {
		name string
		dst  *api.Function
	}{
		{"alloc", &m.allocFn},
		{"dealloc", &m.deallocFn},
		{"plugin_manifest", &m.manifestFn},
		{"tool_execute", &m.toolExecuteFn},
	} {
		f := mod.ExportedFunction(fn.name)
		if f == nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("missing export %q", fn.name)
		}
		*fn.dst = f
	}
	return m, nil
}

// echoStdgoWASM is the standard-Go (Go 1.26 wasip1 reactor) build of
// the echo plugin. Lives next to echoWASM but is built via:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
//	    -o internal/plugins/wasmhost/testdata/echo-stdgo.wasm \
//	    ./examples/wasm-plugin-echo-stdgo/
//
// Embedded to keep the benchmark hermetic — checked-in .wasm bytes
// mean we don't need TinyGo+stdgo on every runner.
//
//go:embed testdata/echo-stdgo.wasm
var echoStdgoWASM []byte

// BenchmarkLoadAndManifest_TinyGo measures the full cold path: compile
// the WASM module, instantiate it, call plugin_manifest once, decode
// the JSON. This is the install-time + first-call latency a user pays
// when a plugin is first loaded after daemon boot.
func BenchmarkLoadAndManifest_TinyGo(b *testing.B) {
	benchLoadAndManifest(b, echoWASM, "com.example.echo", false)
}

// BenchmarkLoadAndManifest_StdGo is the comparison case — the same
// plugin shape compiled with standard Go's wasip1/c-shared reactor mode.
// The reactor variant requires _initialize before exports work, so
// the loader gets a different start-functions config.
func BenchmarkLoadAndManifest_StdGo(b *testing.B) {
	benchLoadAndManifest(b, echoStdgoWASM, "com.example.echo-stdgo", true)
}

func benchLoadAndManifest(b *testing.B, wasm []byte, pluginID string, reactor bool) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx := context.Background()
		// New loader per iteration so we measure cold-start (compile +
		// instantiate), not warm reuse — this matches what an install
		// flow actually pays.
		loader := NewLoader(ctx)
		mod, err := loadForBench(ctx, loader, pluginID, wasm, reactor)
		if err != nil {
			b.Fatalf("Load: %v", err)
		}
		if _, err := mod.Manifest(ctx); err != nil {
			b.Fatalf("Manifest: %v", err)
		}
		_ = mod.Close(ctx)
		_ = loader.Close(ctx)
	}
}

// BenchmarkCallTool_TinyGo measures steady-state per-call cost once the
// module is loaded — what every subsequent tool invocation pays.
func BenchmarkCallTool_TinyGo(b *testing.B) {
	benchCallTool(b, echoWASM, "com.example.echo", false)
}

func BenchmarkCallTool_StdGo(b *testing.B) {
	benchCallTool(b, echoStdgoWASM, "com.example.echo-stdgo", true)
}

func benchCallTool(b *testing.B, wasm []byte, pluginID string, reactor bool) {
	ctx := context.Background()
	loader := NewLoader(ctx)
	defer func() { _ = loader.Close(ctx) }()
	mod, err := loadForBench(ctx, loader, pluginID, wasm, reactor)
	if err != nil {
		b.Fatalf("Load: %v", err)
	}
	defer func() { _ = mod.Close(ctx) }()

	input := map[string]any{"message": "ping", "n": float64(42)}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := mod.CallTool(ctx, "echo.echo", input, nil); err != nil {
			b.Fatalf("CallTool: %v", err)
		}
	}
}
