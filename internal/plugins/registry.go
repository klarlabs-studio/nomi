package plugins

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"go.klarlabs.de/nomi/internal/tools"
)

// Registry is the single source of truth for registered plugins. Role-view
// accessors (Channels, Tools, Triggers, ContextSources) are projections
// built at Register time via type-assertion, so lookups are O(1) and
// don't need to re-check interfaces on every call.
//
// The registry does not own plugin lifecycle (Start/Stop/Configure) —
// that's the caller's responsibility. What it owns: uniqueness, manifest
// validation, and the role fan-out.
type Registry struct {
	mu sync.RWMutex

	// plugins is the authoritative store keyed by PluginManifest.ID.
	plugins map[string]Plugin

	// Per-role projections. Each entry references a plugin stored above;
	// these slices are rebuilt atomically inside Register/Unregister so
	// readers get a consistent snapshot under RLock.
	channelProviders       []ChannelProvider
	toolProviders          []ToolProvider
	triggerProviders       []TriggerProvider
	webhookReceivers       []WebhookReceiver
	contextSourceProviders []ContextSourceProvider

	// Derived lookup: capability → plugin ID. Populated from each
	// manifest's declared capabilities at Register time so the runtime
	// can ask "which plugin provides `gmail.send`?" without scanning.
	capabilityOwners map[string]string
}

// NewRegistry constructs an empty plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		plugins:          make(map[string]Plugin),
		capabilityOwners: make(map[string]string),
	}
}

// foundationCapabilities are tool-level capabilities that represent shared
// permission ceilings rather than plugin-specific ownership. Multiple plugins
// may legitimately need to write to the filesystem or make outbound network
// calls, so these are explicitly exempted from the uniqueness check below.
// Plugin-specific capabilities like "gmail.send" or "github.read" remain
// strictly owner-assigned.
var foundationCapabilities = map[string]bool{
	"filesystem.read":  true,
	"filesystem.write": true,
	"network.outgoing": true,
	"command.exec":     true,
}

// Register adds a plugin to the registry and wires its contributions into
// the appropriate role views. Validates:
//
//   - Plugin ID is non-empty and unique
//   - At least one contribution kind is declared (see Contributions.HasRole)
//   - Every ToolContribution's Capability is present in the manifest's
//     Capabilities list (prevents a plugin from silently declaring a
//     capability via tool that wasn't in its ceiling)
//   - No capability is already claimed by a different plugin (prevents
//     two plugins both claiming `gmail.send`)
//
// Validation is strict and happens before any state mutation, so failed
// Register calls leave the registry unchanged.
func (r *Registry) Register(p Plugin) error {
	if p == nil {
		return fmt.Errorf("plugin is nil")
	}
	manifest := p.Manifest()

	if err := validateManifest(manifest); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.plugins[manifest.ID]; exists {
		return fmt.Errorf("plugin %q already registered", manifest.ID)
	}
	// Only enforce uniqueness for capabilities backing a tool — those
	// represent ownership ("gmail.send is implemented by the Gmail
	// plugin"). Capabilities declared at the manifest level are a
	// permission ceiling that multiple plugins legitimately share —
	// network.outgoing, filesystem.read, etc. should be claimable by
	// any plugin that needs them without colliding.
	toolCaps := map[string]struct{}{}
	for _, t := range manifest.Contributes.Tools {
		toolCaps[t.Capability] = struct{}{}
	}
	for cap := range toolCaps {
		if foundationCapabilities[cap] {
			continue
		}
		if owner, claimed := r.capabilityOwners[cap]; claimed {
			return fmt.Errorf("capability %q already provided by plugin %q", cap, owner)
		}
	}

	r.plugins[manifest.ID] = p
	for cap := range toolCaps {
		if foundationCapabilities[cap] {
			continue
		}
		r.capabilityOwners[cap] = manifest.ID
	}

	// Role projections — type-assert once and cache the references.
	if cp, ok := p.(ChannelProvider); ok && len(manifest.Contributes.Channels) > 0 {
		r.channelProviders = append(r.channelProviders, cp)
	}
	if tp, ok := p.(ToolProvider); ok && len(manifest.Contributes.Tools) > 0 {
		r.toolProviders = append(r.toolProviders, tp)
	}
	if trp, ok := p.(TriggerProvider); ok && len(manifest.Contributes.Triggers) > 0 {
		r.triggerProviders = append(r.triggerProviders, trp)
	}
	if wr, ok := p.(WebhookReceiver); ok {
		r.webhookReceivers = append(r.webhookReceivers, wr)
	}
	if csp, ok := p.(ContextSourceProvider); ok && len(manifest.Contributes.ContextSources) > 0 {
		r.contextSourceProviders = append(r.contextSourceProviders, csp)
	}

	return nil
}

// Unregister removes a plugin from the registry. If the plugin is still
// running the caller is expected to have Stopped it first — Unregister
// doesn't call Stop, because the caller may have broader shutdown
// orchestration to do. Returns an error if the plugin isn't registered.
func (r *Registry) Unregister(pluginID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	p, exists := r.plugins[pluginID]
	if !exists {
		return fmt.Errorf("plugin %q not registered", pluginID)
	}
	manifest := p.Manifest()

	delete(r.plugins, pluginID)
	// Mirror Register: only tool-implemented capabilities are tracked
	// in capabilityOwners, so only those need cleanup here.
	for _, t := range manifest.Contributes.Tools {
		if r.capabilityOwners[t.Capability] == pluginID {
			delete(r.capabilityOwners, t.Capability)
		}
	}

	// Rebuild role slices, dropping any that point at the removed plugin.
	r.channelProviders = filterByPluginID(r.channelProviders, pluginID)
	r.toolProviders = filterByPluginID(r.toolProviders, pluginID)
	r.triggerProviders = filterByPluginID(r.triggerProviders, pluginID)
	r.webhookReceivers = filterByPluginID(r.webhookReceivers, pluginID)
	r.contextSourceProviders = filterByPluginID(r.contextSourceProviders, pluginID)

	return nil
}

// Get returns a plugin by ID, or (nil, error) if not registered.
func (r *Registry) Get(pluginID string) (Plugin, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[pluginID]
	if !ok {
		return nil, fmt.Errorf("plugin %q not registered", pluginID)
	}
	return p, nil
}

// List returns every registered plugin in stable id-sorted order.
// The slice is a fresh copy so callers may iterate without holding
// the registry lock. Sorting at this layer means UI list renders are
// stable across refetches (no card-shuffle on every poll), and
// tests + MCP clients downstream get a deterministic contract for
// free.
func (r *Registry) List() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.plugins))
	for id := range r.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Plugin, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.plugins[id])
	}
	return out
}

// CapabilityOwner reports which plugin declares a capability, if any.
// Used by the runtime's permission-intersection step to point "gmail.send
// denied" back at the owning plugin for diagnostics.
func (r *Registry) CapabilityOwner(cap string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owner, ok := r.capabilityOwners[cap]
	return owner, ok
}

// Channels returns every Channel instance across all registered plugins
// that implement ChannelProvider and contribute at least one channel
// role. Each Channel is scoped to a specific Connection; the runtime
// uses (plugin_id, connection_id) to route outbound messages.
//
// Calling this invokes ChannelProvider.Channels() on every provider —
// channels may be reconfigured between calls, so the result is always
// fresh rather than cached.
func (r *Registry) Channels() []Channel {
	r.mu.RLock()
	providers := append([]ChannelProvider(nil), r.channelProviders...)
	r.mu.RUnlock()

	var out []Channel
	for _, cp := range providers {
		out = append(out, cp.Channels()...)
	}
	return out
}

// Triggers returns every Trigger instance across registered plugins.
// Semantics match Channels — fresh each call, one entry per configured
// trigger per Connection.
func (r *Registry) Triggers() []Trigger {
	r.mu.RLock()
	providers := append([]TriggerProvider(nil), r.triggerProviders...)
	r.mu.RUnlock()

	var out []Trigger
	for _, tp := range providers {
		out = append(out, tp.Triggers()...)
	}
	return out
}

// ContextSources returns every ContextSource instance across registered
// plugins. Semantics match Channels.
func (r *Registry) ContextSources() []ContextSource {
	r.mu.RLock()
	providers := append([]ContextSourceProvider(nil), r.contextSourceProviders...)
	r.mu.RUnlock()

	var out []ContextSource
	for _, csp := range providers {
		out = append(out, csp.ContextSources()...)
	}
	return out
}

// Tools returns every Tool contributed by any ToolProvider. Unlike
// Channels/Triggers/ContextSources, tools are not per-Connection — one
// ToolProvider contributes the same tool set regardless of how many
// Connections the plugin has. Connection-specificity for tool calls is
// handled by the connection_id input parameter, not by instantiating
// tools per-Connection.
func (r *Registry) Tools() []tools.Tool {
	r.mu.RLock()
	providers := append([]ToolProvider(nil), r.toolProviders...)
	r.mu.RUnlock()

	var out []tools.Tool
	for _, tp := range providers {
		out = append(out, tp.Tools()...)
	}
	return out
}

// RegisterToolsInto copies every tool contributed by registered plugins
// into the supplied tools.Registry. The runtime uses this to project
// plugin-contributed tools into the existing tools.Registry used by the
// executor — no changes to executor semantics are required. System
// tools (filesystem.read, command.exec, llm.chat) continue to register
// directly into tools.Registry as they do today; plugin-contributed
// tools join the same pool.
//
// Returns the first registration error. Partial state is possible on
// failure — the caller should treat an error as fatal at startup.
func (r *Registry) RegisterToolsInto(dst *tools.Registry) error {
	for _, t := range r.Tools() {
		if err := dst.Register(t); err != nil {
			return fmt.Errorf("plugin tool %q: %w", t.Name(), err)
		}
	}
	return nil
}

// ManifestCapabilities returns the declared capability list for a plugin.
// Replaces today's connectors.Registry.ManifestCapabilities, which the
// runtime uses as the per-connector permission ceiling.
func (r *Registry) ManifestCapabilities(pluginID string) ([]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[pluginID]
	if !ok {
		return nil, false
	}
	return p.Manifest().Capabilities, true
}

// CapabilitiesForSource resolves a Run's Source value ("telegram",
// "email", "slack", …) to the capability ceiling of the plugin that
// contributes that channel kind. Mirrors today's connector-based lookup
// semantics: the runtime's permission intersection keys off run.Source,
// which is the channel kind (not the plugin ID), so a migration from
// connectors.Registry.ManifestCapabilities → this method is drop-in.
//
// Returns (nil, false) if no registered plugin contributes a channel of
// the given kind, matching the existing contract where a missing lookup
// denies every capability.
func (r *Registry) CapabilitiesForSource(source string) ([]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.plugins {
		m := p.Manifest()
		for _, ch := range m.Contributes.Channels {
			if ch.Kind == source {
				return m.Capabilities, true
			}
		}
	}
	return nil, false
}

// EnabledLookup is the contract pluginRegistry uses to consult per-plugin
// enable/disable state without taking a hard dependency on the storage
// layer. Returning true (or any error) defaults to "start the plugin"
// so a misbehaving lookup never strands a critical channel.
type EnabledLookup func(pluginID string) bool

// StartAll calls Start on every registered plugin. Errors are collected
// but do not short-circuit: a failing plugin doesn't prevent others from
// starting, mirroring today's connectors.Registry.StartAll. The returned
// error is a joined summary of every failure.
//
// When enabled is non-nil, plugins whose enabled lookup returns false
// are skipped. Used by the daemon to honor the user's per-plugin
// enable/disable choice from the plugin_state table (ADR 0002 §1).
func (r *Registry) StartAll(ctx context.Context, enabled ...EnabledLookup) error {
	r.mu.RLock()
	plugins := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		plugins = append(plugins, p)
	}
	r.mu.RUnlock()

	var lookup EnabledLookup
	if len(enabled) > 0 {
		lookup = enabled[0]
	}

	var errs []error
	for _, p := range plugins {
		id := p.Manifest().ID
		if lookup != nil && !lookup(id) {
			// Plugin disabled in plugin_state — skip Start; the registry
			// row stays so the UI can still surface it as "disabled."
			continue
		}
		if err := p.Start(ctx); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", id, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return joinErrors(errs)
}

// StopAll calls Stop on every registered plugin in reverse registration
// order, collecting errors. Mirrors StartAll's semantics.
func (r *Registry) StopAll() error {
	r.mu.RLock()
	plugins := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		plugins = append(plugins, p)
	}
	r.mu.RUnlock()

	var errs []error
	for _, p := range plugins {
		if err := p.Stop(); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", p.Manifest().ID, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return joinErrors(errs)
}

// validateManifest checks plugin-level invariants that apply regardless
// of which role interfaces the plugin implements.
func validateManifest(m PluginManifest) error {
	if m.ID == "" {
		return fmt.Errorf("plugin manifest missing ID")
	}
	if m.Name == "" {
		return fmt.Errorf("plugin %q: manifest missing Name", m.ID)
	}
	if m.Version == "" {
		return fmt.Errorf("plugin %q: manifest missing Version", m.ID)
	}
	switch m.Cardinality {
	case ConnectionSingle, ConnectionMulti, ConnectionMultiMulti:
	case "":
		return fmt.Errorf("plugin %q: manifest missing Cardinality", m.ID)
	default:
		return fmt.Errorf("plugin %q: invalid Cardinality %q", m.ID, m.Cardinality)
	}

	// At least one contribution is required — a plugin that contributes
	// nothing is just dead weight in the registry.
	if !m.Contributes.HasRole("channel") &&
		!m.Contributes.HasRole("tool") &&
		!m.Contributes.HasRole("trigger") &&
		!m.Contributes.HasRole("context_source") {
		return fmt.Errorf("plugin %q: manifest contributes no roles", m.ID)
	}

	// Every tool's capability must be in the plugin's capability list,
	// otherwise the plugin could smuggle in capabilities past the ceiling.
	capSet := make(map[string]struct{}, len(m.Capabilities))
	for _, c := range m.Capabilities {
		capSet[c] = struct{}{}
	}
	for _, t := range m.Contributes.Tools {
		if t.Name == "" {
			return fmt.Errorf("plugin %q: tool contribution has empty Name", m.ID)
		}
		if t.Capability == "" {
			return fmt.Errorf("plugin %q: tool %q has empty Capability", m.ID, t.Name)
		}
		if _, ok := capSet[t.Capability]; !ok {
			return fmt.Errorf("plugin %q: tool %q requires capability %q not declared in manifest", m.ID, t.Name, t.Capability)
		}
	}
	return nil
}

func filterByPluginID[T Plugin](providers []T, removeID string) []T {
	out := providers[:0]
	for _, p := range providers {
		if p.Manifest().ID != removeID {
			out = append(out, p)
		}
	}
	return out
}

// joinErrors formats a slice of errors as a single error preserving each
// message on its own line. We don't use errors.Join here because we want
// explicit numbering in the message to help diagnose startup failures.
func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	msg := fmt.Sprintf("%d plugin errors:", len(errs))
	for i, e := range errs {
		msg += fmt.Sprintf("\n  %d. %s", i+1, e.Error())
	}
	return fmt.Errorf("%s", msg)
}
