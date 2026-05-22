package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memstore"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// EmbeddedClient is the in-process implementation of memstore.Client.
// It wraps a *db.MemoryRepository and translates between the wire-shape
// Scope/Entry types and the domain MemoryEntry rows on disk.
//
// Step 1 of ADR 0004 — once the package extracts to
// github.com/felixgeelhaar/mnemos this struct moves with it; today it
// lives here so the runtime can depend on the memstore.Client interface
// without forcing the repo refactor in the same change.
type EmbeddedClient struct {
	repo     *db.MemoryRepository
	eventBus *events.EventBus
}

// NewEmbeddedClient constructs the embedded backend over the given
// memory repository. The repository must be backed by a writable SQLite
// connection — see internal/storage/db for construction.
//
// The returned client does not emit audit events; call WithEventBus to
// attach an emitter. Production wiring (cmd/nomid) attaches the event
// bus so memory.store / memory.forget / memory.tombstone events feed
// into the hash-chained audit log. Tests typically leave it unset.
func NewEmbeddedClient(repo *db.MemoryRepository) *EmbeddedClient {
	return &EmbeddedClient{repo: repo}
}

// WithEventBus returns the receiver after attaching an event bus for
// audit emission. Returning the receiver keeps the constructor chain
// short at the call site: memory.NewEmbeddedClient(repo).WithEventBus(bus).
func (c *EmbeddedClient) WithEventBus(bus *events.EventBus) *EmbeddedClient {
	c.eventBus = bus
	return c
}

// emit publishes a memory.* audit event when an event bus is attached.
// Best-effort — failure to emit does not roll the underlying memory
// operation back, because the row is already on disk. The error is
// dropped intentionally.
func (c *EmbeddedClient) emit(ctx context.Context, eventType domain.EventType, payload map[string]interface{}) {
	if c.eventBus == nil {
		return
	}
	_, _ = c.eventBus.Publish(ctx, eventType, "", nil, payload)
}

// Compile-time check that EmbeddedClient satisfies memstore.Client.
var _ memstore.Client = (*EmbeddedClient)(nil)

// defaultRetrieveLimit is applied when Query.Limit is zero.
const defaultRetrieveLimit = 50

// Store persists entry under scope. Assigns Entry.ID if empty, stamps
// CreatedAt if zero, and computes ContentHash before returning.
func (c *EmbeddedClient) Store(ctx context.Context, scope memstore.Scope, entry *memstore.Entry) error {
	if entry == nil {
		return fmt.Errorf("memstore.EmbeddedClient.Store: nil entry")
	}
	if err := memstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if entry.ContentHash == "" {
		entry.ContentHash = hashContent(entry.Content)
	}

	row := entryToRow(scope, entry)
	if err := c.repo.Create(row); err != nil {
		return err
	}
	c.emit(ctx, domain.EventMemoryStore, map[string]interface{}{
		"entry_id":     entry.ID,
		"scope_kind":   string(scope.Kind),
		"content_hash": entry.ContentHash,
	})
	return nil
}

// Retrieve returns entries matching query within scope, ordered most
// recent first. Filters happen in-memory after a scope-restricted DB
// read — fine for the corpus sizes we expect; will move to SQL-side
// filtering when the schema gains explicit owner_id / key columns.
func (c *EmbeddedClient) Retrieve(ctx context.Context, scope memstore.Scope, query memstore.Query) ([]*memstore.Entry, error) {
	if err := memstore.ValidateScope(scope); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query.Limit < 0 {
		return nil, fmt.Errorf("memstore.EmbeddedClient.Retrieve: negative limit %d", query.Limit)
	}
	limit := query.Limit
	if limit == 0 {
		limit = defaultRetrieveLimit
	}

	rows, err := c.repo.ListByScope(scopeKindToString(scope.Kind), limit)
	if err != nil {
		return nil, err
	}

	out := make([]*memstore.Entry, 0, len(rows))
	for _, r := range rows {
		if !matchQuery(r, query) {
			continue
		}
		out = append(out, rowToEntry(r))
	}
	return out, nil
}

// Search returns entries whose content matches q within scope. Matches
// today's case-insensitive substring scan; vector retrieval is a
// follow-up (ADR 0004 §11).
func (c *EmbeddedClient) Search(ctx context.Context, scope memstore.Scope, q string, opts memstore.SearchOpts) ([]*memstore.Entry, error) {
	if err := memstore.ValidateScope(scope); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = defaultRetrieveLimit
	}

	// First filter to scope, then in-memory substring match. Mirrors
	// the behavior in Manager.Search to keep the migration semantically
	// transparent.
	rows, err := c.repo.ListByScope(scopeKindToString(scope.Kind), limit)
	if err != nil {
		return nil, err
	}

	q = strings.ToLower(q)
	tokens := strings.Fields(q)
	out := make([]*memstore.Entry, 0, len(rows))
	for _, r := range rows {
		if !contentMatches(r.Content, tokens) {
			continue
		}
		out = append(out, rowToEntry(r))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Forget deletes a single entry by ID within scope. Returns
// memstore.ErrNotFound if no entry exists at that id. Verifies the entry
// belongs to the declared scope before deleting — an out-of-scope id
// is treated as not-found.
func (c *EmbeddedClient) Forget(ctx context.Context, scope memstore.Scope, id string) error {
	if err := memstore.ValidateScope(scope); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" {
		return memstore.ErrNotFound
	}

	row, err := c.repo.GetByID(id)
	if err != nil {
		// Repo returns a wrapped error on missing row; surface as ErrNotFound
		// so callers can errors.Is against the mnemos sentinel.
		if strings.Contains(err.Error(), "not found") {
			return memstore.ErrNotFound
		}
		return err
	}
	if row.Scope != scopeKindToString(scope.Kind) {
		return memstore.ErrNotFound
	}

	if err := c.repo.Delete(id); err != nil {
		return err
	}
	c.emit(ctx, domain.EventMemoryForget, map[string]interface{}{
		"entry_id":     id,
		"scope_kind":   string(scope.Kind),
		"content_hash": hashContent(row.Content),
	})
	return nil
}

// Tombstone anonymizes memory rows referencing the deleted entity.
// Maps to the existing repo-level Anonymize* methods which null out
// the FK column. Idempotent.
func (c *EmbeddedClient) Tombstone(ctx context.Context, ref memstore.EntityRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref.ID == "" {
		return errors.New("memstore.EmbeddedClient.Tombstone: empty ref ID")
	}
	switch ref.Kind {
	case memstore.EntityAssistant:
		if err := c.repo.AnonymizeByAssistant(ref.ID); err != nil {
			return err
		}
	case memstore.EntityRun:
		if err := c.repo.AnonymizeByRun(ref.ID); err != nil {
			return err
		}
	default:
		return fmt.Errorf("memstore.EmbeddedClient.Tombstone: unknown entity kind %q", ref.Kind)
	}
	c.emit(ctx, domain.EventMemoryTombstone, map[string]interface{}{
		"entity_kind": string(ref.Kind),
		"entity_id":   ref.ID,
	})
	return nil
}

// --- conversions ---

// scopeKindToString collapses a memstore.ScopeKind to the flat string
// stored in today's `memory.scope` column. Until step 2 of ADR 0004
// extracts the schema, the column only encodes Kind — OwnerID and Key
// are dropped on persist and reconstituted on read as the local
// defaults.
func scopeKindToString(k memstore.ScopeKind) string {
	return string(k)
}

func entryToRow(scope memstore.Scope, e *memstore.Entry) *domain.MemoryEntry {
	return &domain.MemoryEntry{
		ID:          e.ID,
		Scope:       scopeKindToString(scope.Kind),
		Content:     e.Content,
		AssistantID: e.AssistantID,
		RunID:       e.RunID,
		CreatedAt:   e.CreatedAt,
	}
}

func rowToEntry(r *domain.MemoryEntry) *memstore.Entry {
	return &memstore.Entry{
		ID:          r.ID,
		Content:     r.Content,
		AssistantID: r.AssistantID,
		RunID:       r.RunID,
		CreatedAt:   r.CreatedAt,
		ContentHash: hashContent(r.Content),
	}
}

func matchQuery(r *domain.MemoryEntry, q memstore.Query) bool {
	if q.AssistantID != nil {
		if r.AssistantID == nil || *r.AssistantID != *q.AssistantID {
			return false
		}
	}
	if q.RunID != nil {
		if r.RunID == nil || *r.RunID != *q.RunID {
			return false
		}
	}
	if q.Since != nil && r.CreatedAt.Before(*q.Since) {
		return false
	}
	return true
}

func contentMatches(content string, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	body := strings.ToLower(content)
	for _, t := range tokens {
		if strings.Contains(body, t) {
			return true
		}
	}
	return false
}

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
