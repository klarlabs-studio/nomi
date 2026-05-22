// Package mnemos defines the cognitive-layer boundary between the Nomi
// runtime and its memory subsystem.
//
// The Client interface is the contract every memory implementation must
// satisfy. Today the only implementation lives in internal/memory and
// runs in-process against SQLite (the "embedded" backend). Subsequent
// steps of ADR 0004 will move that implementation to the standalone
// github.com/felixgeelhaar/mnemos repo and add a "remote" HTTP client
// for organizational cognition deployments.
//
// This package contains only the wire-shaped types and the interface.
// It must not import any concrete storage or runtime types — keeping it
// dependency-light is what lets the implementation move out without
// rippling through the rest of Nomi.
//
// See docs/adr/0004-nomi-mnemos-cognitive-boundary.md.
package mnemos
