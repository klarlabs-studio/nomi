package memstore

import (
	"context"
	"errors"
)

// Client is the wire-shaped contract every memory backend implements.
//
// Two backends will exist:
//
//   - embedded: in-process Go calls against a SQLite database. The
//     current internal/memory package becomes the embedded backend once
//     the refactor lands.
//
//   - remote: HTTP + bearer-token client against a Mnemos service. Same
//     interface, network on the other side. Ships in step 3 of ADR 0004.
//
// Implementations must accept context cancellation and return promptly
// when ctx is done. Concrete error values are surfaced as wrapped errors
// (errors.Is(err, ErrNotFound) etc.) so callers can switch on cause
// without depending on backend-specific types.
type Client interface {
	// Store persists entry under scope. The implementation is responsible
	// for assigning Entry.ID if empty, stamping CreatedAt if zero, and
	// computing ContentHash before returning.
	Store(ctx context.Context, scope Scope, entry *Entry) error

	// Retrieve returns entries matching query within scope, ordered most
	// recent first. Implementations honor Query.Limit; zero means the
	// implementation default.
	Retrieve(ctx context.Context, scope Scope, query Query) ([]*Entry, error)

	// Search returns entries whose content matches q within scope. The
	// V1 embedded implementation does case-insensitive substring; future
	// implementations may layer vector retrieval behind the same shape.
	Search(ctx context.Context, scope Scope, q string, opts SearchOpts) ([]*Entry, error)

	// Forget deletes a single entry by ID within scope. Returns
	// ErrNotFound if no entry exists.
	Forget(ctx context.Context, scope Scope, id string) error

	// Tombstone drops or anonymizes entries referencing the named entity.
	// Called by the runtime in response to assistant.deleted / run.deleted
	// events — see ADR 0004 §6. Idempotent: calling Tombstone with an
	// already-tombstoned ref is a no-op.
	Tombstone(ctx context.Context, ref EntityRef) error
}

// Sentinel errors. Backends wrap concrete causes with %w so callers can
// errors.Is against these without importing backend-specific packages.
var (
	// ErrNotFound is returned by Forget when the ID is unknown and by
	// Retrieve-by-id paths when no row matches.
	ErrNotFound = errors.New("mnemos: entry not found")

	// ErrInvalidScope is returned when Scope fails ValidateScope (unknown
	// kind, empty owner, or scope/key mismatch).
	ErrInvalidScope = errors.New("mnemos: invalid scope")

	// ErrScopeMismatch is returned by remote backends when the declared
	// Scope.OwnerID does not match the identity bound to the bearer
	// token. Embedded backends do not return this — see ADR 0004 §3 on
	// embedded-mode trust posture.
	ErrScopeMismatch = errors.New("mnemos: scope owner mismatch")
)

// ValidateScope reports whether s is well-formed. Used by every public
// entry point in implementations to fail fast on caller bugs.
func ValidateScope(s Scope) error {
	if s.OwnerID == "" {
		return ErrInvalidScope
	}
	if !s.Kind.IsValid() {
		return ErrInvalidScope
	}
	switch s.Kind {
	case ScopeWorkspace, ScopeSession, ScopeOrg:
		if s.Key == "" {
			return ErrInvalidScope
		}
	case ScopeProfile, ScopePreferences:
		if s.Key != "" {
			return ErrInvalidScope
		}
	}
	return nil
}
