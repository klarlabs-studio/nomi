// Package memstore is the typed memory boundary between the Nomi runtime
// and its underlying memory store. The Client interface carries a typed
// Scope tuple (owner_id, kind, key) and emits memory.* audit events on
// every Store/Forget/Tombstone — the runtime depends on this, not on
// the concrete repository type.
//
// Historical note: this package was introduced as step 1 of ADR 0004's
// extract-Mnemos-to-its-own-repo plan. ADR 0004 has since been revised
// (2026-05-22): Mnemos is integrated as a plugin (internal/plugins/mnemos)
// rather than as a runtime subsystem. There is no second implementation
// of memstore.Client planned — the EmbeddedClient in internal/memory is
// the only one.
//
// The interface stays because the typed Scope + audit-chain emission +
// tombstone wiring + export/import format that landed alongside it are
// load-bearing for the runtime call sites. See ADR 0005 for the
// keep-vs-revert decision and ADR 0004 for the revised cognitive
// boundary.
//
// This package contains only the wire-shaped types and the interface.
// It must not import any concrete storage or runtime types.
package memstore
