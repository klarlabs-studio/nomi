// Package wasmplugin adapts a wasmhost.Module into a plugins.Plugin so
// marketplace-tier WASM bundles plug into the existing plugin registry
// + tool execution path without parallel infrastructure. Today it only
// surfaces tools; channels / triggers / context sources are
// system-tier-only for v1 (see ADR 0003 §2 — those need richer host
// bridges than the JSON tool ABI provides).
//
// Lifecycle mapping:
//
//	plugins.Plugin.Configure → no-op (WASM plugins receive no
//	                            host-side config; per-call inputs flow
//	                            through tool_execute instead)
//	plugins.Plugin.Start     → no-op (the wasmhost.Module is already
//	                            instantiated when the install handler
//	                            constructs the Plugin)
//	plugins.Plugin.Stop      → closes the underlying wasmhost.Module
//	plugins.Plugin.Status    → reports running until Stop closes it
package wasmplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/tools"
)

// Plugin wraps a wasmhost.Module + a parsed manifest. CallConfigBuilder
// is consulted on every tool call to produce the per-call security
// context the gated host imports check; allowing the host to thread
// assistant policy + permission engine in without coupling this
// package to either of them directly.
//
// Concurrent CallTool calls are serialized inside wasmhost.Module
// (wazero modules aren't reentrant on shared linear memory), so the
// adapter doesn't need extra synchronization on the call path.
type Plugin struct {
	manifest plugins.PluginManifest
	module   *wasmhost.Module
	cfg      func() *wasmhost.CallConfig

	mu      sync.Mutex
	running bool
}

// New wraps an instantiated wasmhost.Module behind the Plugin
// interface. cfgBuilder may be nil — a tool that calls a gated host
// import without a CallConfig will be denied (see wasmhost gates), but
// pure-compute tools work fine without it.
func New(manifest plugins.PluginManifest, mod *wasmhost.Module, cfgBuilder func() *wasmhost.CallConfig) *Plugin {
	return &Plugin{
		manifest: manifest,
		module:   mod,
		cfg:      cfgBuilder,
		running:  true,
	}
}

// Manifest implements plugins.Plugin.
func (p *Plugin) Manifest() plugins.PluginManifest { return p.manifest }

// Configure is a no-op — see package doc. WASM plugins receive their
// per-call input through the tool_execute ABI; there is no host-side
// configure step in the marketplace-tier ABI today.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start is a no-op — the wasmhost.Module is already live when New
// constructed this Plugin.
func (p *Plugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	return nil
}

// Stop closes the underlying wasmhost.Module. After Stop, Tools() will
// still return the same Tool slice but Execute will fail with the
// wasmhost's closed-module error.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	p.running = false
	if p.module != nil {
		return p.module.Close(context.Background())
	}
	return nil
}

// Status reports running until Stop has been called.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return plugins.PluginStatus{Running: p.running, Ready: p.running}
}

// Tools implements plugins.ToolProvider — projects each
// ToolContribution from the manifest into a wasmTool that dispatches
// into the wasmhost on Execute.
func (p *Plugin) Tools() []tools.Tool {
	out := make([]tools.Tool, 0, len(p.manifest.Contributes.Tools))
	for _, t := range p.manifest.Contributes.Tools {
		out = append(out, &wasmTool{
			plugin:     p,
			name:       t.Name,
			capability: t.Capability,
		})
	}
	return out
}

// wasmTool is the per-tool dispatcher. Each ToolContribution becomes
// one of these; Execute routes through the parent Plugin's
// wasmhost.Module so the security gates run with consistent CallConfig.
type wasmTool struct {
	plugin     *Plugin
	name       string
	capability string
}

func (t *wasmTool) Name() string       { return t.name }
func (t *wasmTool) Capability() string { return t.capability }

func (t *wasmTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	if t.plugin == nil || t.plugin.module == nil {
		return nil, fmt.Errorf("wasmplugin: tool %q has no module", t.name)
	}
	var cfg *wasmhost.CallConfig
	if t.plugin.cfg != nil {
		cfg = t.plugin.cfg()
	}
	resp, err := t.plugin.module.CallTool(ctx, t.name, input, cfg)
	if err != nil {
		return nil, fmt.Errorf("wasmplugin: %s: %w", t.name, err)
	}
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		return nil, fmt.Errorf("wasmplugin: %s: %s", t.name, errMsg)
	}
	if result, ok := resp["result"].(map[string]any); ok {
		return result, nil
	}
	// No "result" key but no error either — return the whole response
	// so callers can still surface the data.
	return resp, nil
}
