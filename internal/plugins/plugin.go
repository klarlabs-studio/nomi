package plugins

import (
	"context"
	"encoding/json"
)

// Plugin is the core contract every plugin implements. Role-specific work
// is expressed via the optional interfaces in roles.go (ChannelProvider,
// ToolProvider, TriggerProvider, ContextSourceProvider), which the
// registry type-asserts at registration time. This core interface handles
// the plugin lifecycle only.
//
// A plugin's lifecycle mirrors today's connector lifecycle but with an
// explicit Configure step:
//
//	Configure(ctx, config)  // called whenever a Connection is added or
//	                        // updated; plugin validates and caches state.
//	                        // May be called multiple times before Start
//	                        // or while running.
//	Start(ctx)              // begin background work (polling loops,
//	                        // WebSocket connections, webhook listeners).
//	                        // Idempotent: calling Start on an already-
//	                        // running plugin returns nil.
//	Stop()                  // graceful shutdown. Must return within a
//	                        // reasonable bound (say 5s) so the daemon's
//	                        // shutdown isn't blocked by a hung plugin.
//
// Thread safety: all methods may be called from multiple goroutines.
// Implementations are responsible for their own synchronization; the
// registry does not serialize calls.
type Plugin interface {
	// Manifest returns the plugin's static metadata. Must be deterministic
	// — the registry caches the result and relies on identity across the
	// plugin's lifetime.
	Manifest() PluginManifest

	// Configure is called with the plugin's aggregate configuration (all
	// Connections merged into one document shaped for the plugin's needs).
	// The runtime hot-reloads by calling Configure with the new document;
	// plugins should reconcile in place rather than restarting themselves.
	Configure(ctx context.Context, config json.RawMessage) error

	// Start begins background work. Returns nil on successful start or if
	// the plugin is already running.
	Start(ctx context.Context) error

	// Stop gracefully shuts down. Returns nil if already stopped.
	Stop() error

	// Status returns the current plugin-level status (see PluginStatus).
	Status() PluginStatus
}
