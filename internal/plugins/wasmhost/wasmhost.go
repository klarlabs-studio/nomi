// Package wasmhost loads and runs WebAssembly plugin modules under the
// wazero runtime. Foundation for the marketplace plugin tier described
// in ADR 0002 §2 — system plugins continue to live as native Go code,
// while user-installed (.nomi-plugin) bundles route through this host.
//
// The contract this package implements is the WASM ABI:
//
//	Plugin exports (the WASM module must provide):
//	  alloc(size: i32) -> i32              // host allocates a buffer in plugin memory
//	  dealloc(ptr: i32, size: i32)         // host frees a buffer
//	  plugin_manifest() -> i64             // returns packed (ptr<<32 | len) for JSON manifest
//	  tool_execute(ptr: i32, len: i32) -> i64
//	    // input is JSON {name, input}; output is JSON {result, error}
//	    // returned i64 is packed (ptr<<32 | len) into plugin memory
//
//	Host imports (the runtime exposes; plugin's manifest must declare
//	the matching capability for each):
//	  host_log(level: i32, msg_ptr: i32, msg_len: i32)
//	    // always available
//	  host_emit_event(type_ptr, type_len, payload_ptr, payload_len)
//	    // always available
//
// Subsequent tasks add the gated host imports (host_http_request,
// host_filesystem_read/write, host_command_exec, host_secrets_get) per
// ADR 0002 §3.
package wasmhost

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Module is one loaded plugin instance. Each represents a single
// instantiated WASM module with its own linear memory and its own
// reference into the wazero runtime. Safe for concurrent use as long as
// callers don't issue overlapping calls into the same exported
// function — wazero serializes per-module by default.
type Module struct {
	runtime  wazero.Runtime
	module   api.Module
	pluginID string

	allocFn       api.Function
	deallocFn     api.Function
	manifestFn    api.Function
	toolExecuteFn api.Function
}

// Loader holds the wazero runtime and the host-import builder shared
// across plugin loads. One Loader per daemon process; many Modules per
// Loader.
type Loader struct {
	runtime wazero.Runtime
	// envOnce gates registerHostImports — wazero's
	// NewHostModuleBuilder("env").Instantiate is destructive on a
	// second call (it wipes the first env instance and leaves the
	// runtime with no env, which then breaks every subsequent plugin
	// load). Once-per-loader registration avoids the trap.
	envOnce sync.Once
	envErr  error
}

// NewLoader constructs a Loader with a fresh wazero runtime configured
// for plugin use. Calls Close on the parent context cancellation to
// release the underlying compilation cache.
//
// Registers the WASI Preview 1 host module so plugins compiled with
// TinyGo's `-target=wasi` (the canonical TinyGo WASM target) can link.
// The Nomi-specific host imports (host_log, host_emit_event, …) are
// added on top in registerHostImports during Load.
func NewLoader(ctx context.Context) *Loader {
	rt := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	return &Loader{runtime: rt}
}

// Close releases the runtime + compiled modules. Call during daemon
// shutdown.
func (l *Loader) Close(ctx context.Context) error {
	return l.runtime.Close(ctx)
}

// Load instantiates a WASM module from bytes and returns a Module wrapping
// it. pluginID is recorded for log + error attribution; it doesn't have
// to match the manifest's declared id at this stage (the install flow
// in lifecycle-07 cross-checks them).
//
// All host imports the module needs must be exported under the env
// namespace (which is what TinyGo defaults to). Imports the host
// doesn't provide cause Instantiate to fail with a clear error so a
// malicious or buggy plugin can't smuggle in capabilities the manifest
// didn't request.
func (l *Loader) Load(ctx context.Context, pluginID string, wasm []byte) (*Module, error) {
	l.envOnce.Do(func() {
		l.envErr = registerHostImports(ctx, l.runtime)
	})
	if l.envErr != nil {
		return nil, fmt.Errorf("register host imports: %w", l.envErr)
	}
	// TinyGo's WASI target compiles main() as the WASI _start entry,
	// which wazero auto-invokes on Instantiate by default — that
	// returns exit_code(0) and marks the module closed before we get
	// to call any exports. WithStartFunctions() (no args) overrides
	// the default ["_start"] list so the module stays loaded.
	//
	// WithName(pluginID) gives the module a unique runtime identity so
	// two plugins compiled from byte-identical wasm (e.g. two echo
	// installs in dev) don't collide — wazero rejects duplicate
	// instance names per runtime.
	cfg := wazero.NewModuleConfig().WithStartFunctions().WithName(pluginID)
	compiled, err := l.runtime.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", pluginID, err)
	}
	mod, err := l.runtime.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate %s: %w", pluginID, err)
	}
	m := &Module{
		runtime:  l.runtime,
		module:   mod,
		pluginID: pluginID,
	}
	// Look up required exports up front — fail fast if the plugin is
	// missing one of the four mandatory ABI functions.
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
			return nil, fmt.Errorf("plugin %s missing required export %q", pluginID, fn.name)
		}
		*fn.dst = f
	}
	return m, nil
}

// Close unloads the module's runtime instance. Call during plugin
// uninstall or daemon shutdown.
func (m *Module) Close(ctx context.Context) error {
	if m.module == nil {
		return nil
	}
	return m.module.Close(ctx)
}

// Manifest calls the plugin's plugin_manifest export and decodes the
// returned JSON into a generic map. Higher layers (lifecycle-05) will
// type this against PluginManifest; for the spike a map is sufficient
// to validate the round-trip works.
func (m *Module) Manifest(ctx context.Context) (map[string]any, error) {
	results, err := m.manifestFn.Call(ctx)
	if err != nil {
		return nil, fmt.Errorf("plugin_manifest: %w", err)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("plugin_manifest: expected 1 result, got %d", len(results))
	}
	ptr, length := unpackPtrLen(results[0])
	body, ok := m.module.Memory().Read(ptr, length)
	if !ok {
		return nil, fmt.Errorf("plugin_manifest: failed to read %d bytes at %d", length, ptr)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("plugin_manifest: parse JSON: %w (raw: %s)", err, body)
	}
	// Free the buffer back to the plugin so its allocator can reuse it.
	_, _ = m.deallocFn.Call(ctx, uint64(ptr), uint64(length))
	return out, nil
}

// CallTool invokes the plugin's tool_execute export with a JSON-encoded
// {name, input} payload and returns the decoded {result, error}
// response. cfg threads the per-call security context the gated host
// imports use to apply the capability gates promised by ADR 0002 §3.
// Pass nil only in tests that don't exercise host imports.
func (m *Module) CallTool(ctx context.Context, toolName string, input map[string]any, cfg *CallConfig) (map[string]any, error) {
	if cfg != nil {
		ctx = WithCallConfig(ctx, cfg)
	}
	payload, err := json.Marshal(map[string]any{
		"name":  toolName,
		"input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("encode tool input: %w", err)
	}

	// Allocate a buffer in the plugin's memory space and copy the
	// payload in. The plugin frees this buffer when it's done with it.
	buf, err := m.allocBuffer(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("alloc input buffer: %w", err)
	}

	results, err := m.toolExecuteFn.Call(ctx, uint64(buf.ptr), uint64(buf.length))
	// Free the input regardless of whether the call succeeded.
	_, _ = m.deallocFn.Call(ctx, uint64(buf.ptr), uint64(buf.length))
	if err != nil {
		return nil, fmt.Errorf("tool_execute: %w", err)
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("tool_execute: expected 1 result, got %d", len(results))
	}
	outPtr, outLen := unpackPtrLen(results[0])
	body, ok := m.module.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("tool_execute: failed to read %d bytes at %d", outLen, outPtr)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("tool_execute: parse JSON: %w (raw: %s)", err, body)
	}
	_, _ = m.deallocFn.Call(ctx, uint64(outPtr), uint64(outLen))
	return out, nil
}

// allocBuffer asks the plugin to reserve `len(data)` bytes in its
// memory and copies the data in. Returns the resulting (ptr, len) pair
// so callers can pass them to plugin functions and free them afterward.
type wasmBuffer struct {
	ptr    uint32
	length uint32
}

func (m *Module) allocBuffer(ctx context.Context, data []byte) (wasmBuffer, error) {
	results, err := m.allocFn.Call(ctx, uint64(len(data)))
	if err != nil {
		return wasmBuffer{}, fmt.Errorf("alloc(%d): %w", len(data), err)
	}
	if len(results) != 1 {
		return wasmBuffer{}, fmt.Errorf("alloc: expected 1 result, got %d", len(results))
	}
	ptr := uint32(results[0])
	if !m.module.Memory().Write(ptr, data) {
		_, _ = m.deallocFn.Call(ctx, uint64(ptr), uint64(len(data)))
		return wasmBuffer{}, fmt.Errorf("write(%d bytes @ %d) out of bounds", len(data), ptr)
	}
	return wasmBuffer{ptr: ptr, length: uint32(len(data))}, nil
}

// unpackPtrLen unpacks the i64 return convention used by every export
// that returns a buffer: high 32 bits are the pointer, low 32 bits are
// the length. Avoids the need for multi-value returns which TinyGo's
// WASM target doesn't always support cleanly.
func unpackPtrLen(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed & 0xFFFFFFFF)
}

// PackPtrLen is the host-side encoder symmetric with unpackPtrLen.
// Exposed so test fixtures can build packed values to feed back into
// the module.
func PackPtrLen(ptr, length uint32) uint64 {
	return (uint64(ptr) << 32) | uint64(length)
}

// registerHostImports wires the env-namespace imports the WASM ABI
// expects. Open imports (host_log, host_emit_event) are always
// available; gated imports (host_http_request, host_filesystem_read,
// host_filesystem_write, host_command_exec, host_secrets_get) check
// the per-call CallConfig before performing their work. Both the
// load-time manifest declaration AND the runtime PermissionPolicy
// must allow the operation; missing CallConfig (system-context calls)
// rejects all gated operations.
func registerHostImports(ctx context.Context, runtime wazero.Runtime) error {
	_, err := runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(hostLog).Export("host_log").
		NewFunctionBuilder().WithFunc(hostEmitEvent).Export("host_emit_event").
		NewFunctionBuilder().WithFunc(hostHTTPRequest).Export("host_http_request").
		NewFunctionBuilder().WithFunc(hostFilesystemRead).Export("host_filesystem_read").
		NewFunctionBuilder().WithFunc(hostFilesystemWrite).Export("host_filesystem_write").
		NewFunctionBuilder().WithFunc(hostCommandExec).Export("host_command_exec").
		NewFunctionBuilder().WithFunc(hostSecretsGet).Export("host_secrets_get").
		Instantiate(ctx)
	return err
}

// hostLog echoes a plugin log line through the daemon's standard
// logger. level is informational only for v1 — log routing into the
// event bus comes later.
func hostLog(ctx context.Context, mod api.Module, level int32, msgPtr, msgLen uint32) {
	body, ok := mod.Memory().Read(msgPtr, msgLen)
	if !ok {
		log.Printf("[wasmhost] host_log: bad memory ref %d/%d", msgPtr, msgLen)
		return
	}
	log.Printf("[wasm:%d] %s", level, string(body))
}

// hostEmitEvent is the spike-stage no-op for plugin-emitted events.
// lifecycle-04 wires this through internal/events.EventBus.
func hostEmitEvent(ctx context.Context, mod api.Module, typePtr, typeLen, payloadPtr, payloadLen uint32) {
	tBody, _ := mod.Memory().Read(typePtr, typeLen)
	pBody, _ := mod.Memory().Read(payloadPtr, payloadLen)
	log.Printf("[wasmhost] event %q: %s", string(tBody), string(pBody))
}
