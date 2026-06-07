package memstore

import "time"

// ScopeKind partitions memory by the lifetime and audience of the data.
// In remote mode the server enforces that callers may only address scopes
// whose owner_id matches their bearer-token identity. In embedded mode
// enforcement is advisory (every query includes the scope tuple, which
// catches accidental mis-routing but is not a security boundary; the
// runtime is the trust boundary in that case — see ADR 0004 §3).
type ScopeKind string

const (
	ScopeWorkspace   ScopeKind = "workspace"
	ScopeProfile     ScopeKind = "profile"
	ScopeSession     ScopeKind = "session"
	ScopeOrg         ScopeKind = "org"
	ScopePreferences ScopeKind = "preferences"
)

// IsValid reports whether the scope kind is one of the declared values.
func (k ScopeKind) IsValid() bool {
	switch k {
	case ScopeWorkspace, ScopeProfile, ScopeSession, ScopeOrg, ScopePreferences:
		return true
	}
	return false
}

// Scope identifies a memory partition.
//
// OwnerID names the principal whose memory this is. In embedded mode it
// is the constant LocalOwnerID. In remote mode it is the user or org
// identity bound to the bearer token used at the wire boundary.
//
// Key qualifies the scope within its kind — a workspace path for
// ScopeWorkspace, a session ID for ScopeSession, an org slug for
// ScopeOrg, and the empty string for ScopeProfile.
type Scope struct {
	OwnerID string    `json:"owner_id"`
	Kind    ScopeKind `json:"kind"`
	Key     string    `json:"key,omitempty"`
}

// LocalOwnerID is the OwnerID used by the embedded backend on a
// single-user laptop. Remote backends derive OwnerID from authentication.
const LocalOwnerID = "local"

// DefaultWorkspaceKey is the placeholder Key used for ScopeWorkspace
// until per-workspace partitioning lands. Today the embedded backend
// stores all workspace memory in one bucket; the constant exists so
// callers don't sprinkle the literal string and so the eventual move
// to real workspace paths is a one-spot change.
const DefaultWorkspaceKey = "default"

// LocalWorkspace returns the canonical Scope for embedded-mode
// workspace memory. Equivalent to today's `scope="workspace"` string.
func LocalWorkspace() Scope {
	return Scope{OwnerID: LocalOwnerID, Kind: ScopeWorkspace, Key: DefaultWorkspaceKey}
}

// LocalProfile returns the canonical Scope for embedded-mode profile
// memory. Equivalent to today's `scope="profile"` string.
func LocalProfile() Scope {
	return Scope{OwnerID: LocalOwnerID, Kind: ScopeProfile}
}

// LocalPreferences returns the canonical Scope for embedded-mode
// per-assistant learned preferences (e.g. plan-edit feedback the
// runtime mines into planner hints). Equivalent to today's
// `scope="preferences"` string.
func LocalPreferences() Scope {
	return Scope{OwnerID: LocalOwnerID, Kind: ScopePreferences}
}

// Entry is one stored memory.
//
// Refs back to the run or assistant that produced the entry are kept by
// reference, not by foreign key — once Mnemos owns its own database file
// (ADR 0004 §6) FK enforcement across DBs is impossible. Tombstone()
// drives cleanup when the referenced run or assistant is deleted.
type Entry struct {
	ID          string     `json:"id"`
	Content     string     `json:"content"`
	AssistantID *string    `json:"assistant_id,omitempty"`
	RunID       *string    `json:"run_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`

	// ContentHash is set by the implementation on Store. Surfaces in the
	// audit-log "memory.op" event so the chain can verify what was written
	// without re-reading from the store.
	ContentHash string `json:"content_hash,omitempty"`
}

// Query is the predicate set for Retrieve.
//
// Limit is the maximum number of entries to return. Zero means
// implementation default (typically 50). Negative values are rejected.
type Query struct {
	AssistantID *string    `json:"assistant_id,omitempty"`
	RunID       *string    `json:"run_id,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	Limit       int        `json:"limit,omitempty"`
}

// SearchOpts controls semantic search. For the V1 implementation
// (substring scan) only Limit is honored. Vector retrieval and ranking
// land in a follow-up; see ADR 0004 §11.
type SearchOpts struct {
	Limit int `json:"limit,omitempty"`
}

// EntityKind names the upstream domain object whose deletion should
// tombstone associated memory rows.
type EntityKind string

const (
	EntityAssistant EntityKind = "assistant"
	EntityRun       EntityKind = "run"
)

// EntityRef points at a deleted upstream entity. Passed to Tombstone()
// so the memory implementation can drop or anonymize associated rows.
type EntityRef struct {
	Kind EntityKind `json:"kind"`
	ID   string     `json:"id"`
}
