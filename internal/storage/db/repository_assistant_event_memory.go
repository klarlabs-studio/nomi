package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// AssistantRepository handles database operations for Assistants
type AssistantRepository struct {
	db *DB
}

// NewAssistantRepository creates a new AssistantRepository
func NewAssistantRepository(db *DB) *AssistantRepository {
	return &AssistantRepository{db: db}
}

// Create inserts a new assistant
func (r *AssistantRepository) Create(assistant *domain.AssistantDefinition) error {
	channels, err := json.Marshal(assistant.Channels)
	if err != nil {
		return fmt.Errorf("failed to marshal channels: %w", err)
	}

	capabilities, err := json.Marshal(assistant.Capabilities)
	if err != nil {
		return fmt.Errorf("failed to marshal capabilities: %w", err)
	}

	contexts, err := json.Marshal(assistant.Contexts)
	if err != nil {
		return fmt.Errorf("failed to marshal contexts: %w", err)
	}

	memoryPolicy, err := json.Marshal(assistant.MemoryPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal memory policy: %w", err)
	}

	permissionPolicy, err := json.Marshal(assistant.PermissionPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal permission policy: %w", err)
	}

	modelPolicy, err := json.Marshal(assistant.ModelPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal model policy: %w", err)
	}

	channelConfigs, err := json.Marshal(assistant.ChannelConfigs)
	if err != nil {
		return fmt.Errorf("failed to marshal channel configs: %w", err)
	}

	query := `
		INSERT INTO assistants (
			id, template_id, name, tagline, role, best_for, not_for, suggested_model,
			system_prompt, channels, channel_configs, capabilities, contexts, memory_policy,
			permission_policy, model_policy, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = r.db.Exec(query,
		assistant.ID, assistant.TemplateID, assistant.Name, assistant.Tagline,
		assistant.Role, assistant.BestFor, assistant.NotFor, assistant.SuggestedModel, assistant.SystemPrompt,
		channels, channelConfigs, capabilities, contexts, memoryPolicy, permissionPolicy, modelPolicy,
		assistant.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create assistant: %w", err)
	}
	return nil
}

// GetByID retrieves an assistant by ID
func (r *AssistantRepository) GetByID(id string) (*domain.AssistantDefinition, error) {
	query := `
		SELECT id, template_id, name, tagline, role, best_for, not_for, suggested_model, system_prompt,
		       channels, channel_configs, capabilities, contexts, memory_policy, permission_policy, model_policy, created_at
		FROM assistants WHERE id = ?
	`
	assistant := &domain.AssistantDefinition{}

	var channels, channelConfigs, capabilities, contexts, memoryPolicy, permissionPolicy, modelPolicy []byte

	err := r.db.QueryRow(query, id).Scan(
		&assistant.ID, &assistant.TemplateID, &assistant.Name, &assistant.Tagline,
		&assistant.Role, &assistant.BestFor, &assistant.NotFor, &assistant.SuggestedModel, &assistant.SystemPrompt,
		&channels, &channelConfigs, &capabilities, &contexts, &memoryPolicy, &permissionPolicy, &modelPolicy,
		&assistant.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("assistant not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get assistant: %w", err)
	}

	if err := json.Unmarshal(channels, &assistant.Channels); err != nil {
		return nil, fmt.Errorf("failed to unmarshal channels: %w", err)
	}
	if len(channelConfigs) > 0 {
		if err := json.Unmarshal(channelConfigs, &assistant.ChannelConfigs); err != nil {
			return nil, fmt.Errorf("failed to unmarshal channel configs: %w", err)
		}
	}
	if err := json.Unmarshal(capabilities, &assistant.Capabilities); err != nil {
		return nil, fmt.Errorf("failed to unmarshal capabilities: %w", err)
	}
	if err := json.Unmarshal(contexts, &assistant.Contexts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal contexts: %w", err)
	}
	if err := json.Unmarshal(memoryPolicy, &assistant.MemoryPolicy); err != nil {
		return nil, fmt.Errorf("failed to unmarshal memory policy: %w", err)
	}
	if err := json.Unmarshal(permissionPolicy, &assistant.PermissionPolicy); err != nil {
		return nil, fmt.Errorf("failed to unmarshal permission policy: %w", err)
	}
	if len(modelPolicy) > 0 {
		var mp domain.ModelPolicy
		if err := json.Unmarshal(modelPolicy, &mp); err == nil {
			assistant.ModelPolicy = &mp
		}
	}

	return assistant, nil
}

// Update updates an assistant
func (r *AssistantRepository) Update(assistant *domain.AssistantDefinition) error {
	channels, err := json.Marshal(assistant.Channels)
	if err != nil {
		return fmt.Errorf("failed to marshal channels: %w", err)
	}

	capabilities, err := json.Marshal(assistant.Capabilities)
	if err != nil {
		return fmt.Errorf("failed to marshal capabilities: %w", err)
	}

	contexts, err := json.Marshal(assistant.Contexts)
	if err != nil {
		return fmt.Errorf("failed to marshal contexts: %w", err)
	}

	memoryPolicy, err := json.Marshal(assistant.MemoryPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal memory policy: %w", err)
	}

	permissionPolicy, err := json.Marshal(assistant.PermissionPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal permission policy: %w", err)
	}

	modelPolicy, err := json.Marshal(assistant.ModelPolicy)
	if err != nil {
		return fmt.Errorf("failed to marshal model policy: %w", err)
	}

	channelConfigs, err := json.Marshal(assistant.ChannelConfigs)
	if err != nil {
		return fmt.Errorf("failed to marshal channel configs: %w", err)
	}

	query := `
		UPDATE assistants
		SET template_id = ?, name = ?, tagline = ?, role = ?, best_for = ?, not_for = ?, suggested_model = ?,
		    system_prompt = ?, channels = ?, channel_configs = ?, capabilities = ?, contexts = ?,
		    memory_policy = ?, permission_policy = ?, model_policy = ?
		WHERE id = ?
	`
	_, err = r.db.Exec(query,
		assistant.TemplateID, assistant.Name, assistant.Tagline, assistant.Role, assistant.BestFor, assistant.NotFor,
		assistant.SuggestedModel, assistant.SystemPrompt,
		channels, channelConfigs, capabilities, contexts, memoryPolicy, permissionPolicy, modelPolicy,
		assistant.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update assistant: %w", err)
	}
	return nil
}

// List retrieves all assistants
func (r *AssistantRepository) List(limit, offset int) ([]*domain.AssistantDefinition, error) {
	query := `
		SELECT id, template_id, name, tagline, role, best_for, not_for, suggested_model, system_prompt,
		       channels, channel_configs, capabilities, contexts, memory_policy, permission_policy, model_policy, created_at
		FROM assistants ORDER BY created_at DESC LIMIT ? OFFSET ?
	`
	rows, err := r.db.Query(query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to list assistants: %w", err)
	}
	defer rows.Close()

	return r.scanAssistants(rows)
}

// scanAssistants scans rows into AssistantDefinition structs
func (r *AssistantRepository) scanAssistants(rows *sql.Rows) ([]*domain.AssistantDefinition, error) {
	assistants := make([]*domain.AssistantDefinition, 0)

	for rows.Next() {
		assistant := &domain.AssistantDefinition{}
		var channels, channelConfigs, capabilities, contexts, memoryPolicy, permissionPolicy, modelPolicy []byte

		err := rows.Scan(
			&assistant.ID, &assistant.TemplateID, &assistant.Name, &assistant.Tagline,
			&assistant.Role, &assistant.BestFor, &assistant.NotFor, &assistant.SuggestedModel, &assistant.SystemPrompt,
			&channels, &channelConfigs, &capabilities, &contexts, &memoryPolicy, &permissionPolicy, &modelPolicy,
			&assistant.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan assistant: %w", err)
		}

		if err := json.Unmarshal(channels, &assistant.Channels); err != nil {
			return nil, fmt.Errorf("failed to unmarshal channels: %w", err)
		}
		if len(channelConfigs) > 0 {
			if err := json.Unmarshal(channelConfigs, &assistant.ChannelConfigs); err != nil {
				return nil, fmt.Errorf("failed to unmarshal channel configs: %w", err)
			}
		}
		if err := json.Unmarshal(capabilities, &assistant.Capabilities); err != nil {
			return nil, fmt.Errorf("failed to unmarshal capabilities: %w", err)
		}
		if err := json.Unmarshal(contexts, &assistant.Contexts); err != nil {
			return nil, fmt.Errorf("failed to unmarshal contexts: %w", err)
		}
		if err := json.Unmarshal(memoryPolicy, &assistant.MemoryPolicy); err != nil {
			return nil, fmt.Errorf("failed to unmarshal memory policy: %w", err)
		}
		if err := json.Unmarshal(permissionPolicy, &assistant.PermissionPolicy); err != nil {
			return nil, fmt.Errorf("failed to unmarshal permission policy: %w", err)
		}
		if len(modelPolicy) > 0 {
			var mp domain.ModelPolicy
			if err := json.Unmarshal(modelPolicy, &mp); err == nil {
				assistant.ModelPolicy = &mp
			}
		}

		assistants = append(assistants, assistant)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return assistants, nil
}

// Delete removes an assistant
func (r *AssistantRepository) Delete(id string) error {
	_, err := r.db.Exec("DELETE FROM assistants WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete assistant: %w", err)
	}
	return nil
}

// EventRepository handles database operations for Events.
//
// Writes are serialized through `chainMu` so the hash-chain (see
// migration 000023) has a well-defined predecessor for every entry,
// even under concurrent producers. The mutex is process-local — Nomi
// is a single-writer daemon, so this is sufficient for the audit
// guarantee. Multi-writer setups would need a chain-coordinator service.
type EventRepository struct {
	db      *DB
	chainMu sync.Mutex
}

// NewEventRepository creates a new EventRepository
func NewEventRepository(db *DB) *EventRepository {
	return &EventRepository{db: db}
}

// Create inserts a new event
func (r *EventRepository) Create(event *domain.Event) error {
	r.chainMu.Lock()
	defer r.chainMu.Unlock()
	prevHash, err := lookupLatestEntryHashDB(r.db)
	if err != nil {
		return fmt.Errorf("failed to read prior entry_hash: %w", err)
	}
	return r.create(r.db, event, prevHash)
}

// CreateTx inserts an event inside the caller's transaction. Used by the
// runtime so state-machine row updates and their corresponding events are
// persisted atomically. Still serializes on chainMu to keep the hash chain
// well-defined.
func (r *EventRepository) CreateTx(tx *sql.Tx, event *domain.Event) error {
	r.chainMu.Lock()
	defer r.chainMu.Unlock()
	prevHash, err := lookupLatestEntryHashTx(tx)
	if err != nil {
		return fmt.Errorf("failed to read prior entry_hash: %w", err)
	}
	return r.create(tx, event, prevHash)
}

func (r *EventRepository) create(e execer, event *domain.Event, prevHash string) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	entryHash := computeEntryHash(prevHash, event, payload)

	query := `
		INSERT INTO events (id, type, run_id, step_id, payload, timestamp, prev_hash, entry_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := e.Exec(query,
		event.ID, event.Type, nullableString(event.RunID), event.StepID,
		payload, event.Timestamp, nullableString(prevHash), entryHash,
	); err != nil {
		return fmt.Errorf("failed to create event: %w", err)
	}
	return nil
}

// queryRower lets us share the latest-hash lookup between the
// connection (DB) and transaction (sql.Tx) variants of Create.
type queryRower interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

const latestEntryHashQuery = `
	SELECT entry_hash FROM events
	WHERE entry_hash IS NOT NULL
	ORDER BY timestamp DESC, id DESC
	LIMIT 1
`

func lookupLatestEntryHashDB(db *DB) (string, error) {
	return scanLatestEntryHash(db.QueryRow(latestEntryHashQuery))
}

func lookupLatestEntryHashTx(tx *sql.Tx) (string, error) {
	return scanLatestEntryHash(tx.QueryRow(latestEntryHashQuery))
}

func scanLatestEntryHash(row *sql.Row) (string, error) {
	var hash sql.NullString
	if err := row.Scan(&hash); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	if !hash.Valid {
		return "", nil
	}
	return hash.String, nil
}

// computeEntryHash returns sha256_hex(prev_hash || canonical_event).
// canonical_event encodes id, type, run_id, step_id, timestamp (UTC,
// RFC3339Nano), and the marshalled payload bytes — anything an auditor
// can read back from the row. Sorting keys keeps the hash stable across
// Go versions and JSON encoder tweaks.
func computeEntryHash(prevHash string, event *domain.Event, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	canon := canonicalEventBytes(event, payload)
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalEventBytes(event *domain.Event, payload []byte) []byte {
	var stepID string
	if event.StepID != nil {
		stepID = *event.StepID
	}
	// Map keys are written in sorted order so the encoder output is
	// stable independent of Go's map iteration randomness.
	fields := map[string]string{
		"id":        event.ID,
		"type":      string(event.Type),
		"run_id":    event.RunID,
		"step_id":   stepID,
		"timestamp": event.Timestamp.UTC().Format(time.RFC3339Nano),
		"payload":   string(payload),
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []byte
	for _, k := range keys {
		out = append(out, k...)
		out = append(out, '=')
		out = append(out, fields[k]...)
		out = append(out, '\n')
	}
	return out
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// VerifyChain walks the events table in canonical order (timestamp ASC,
// id ASC) and recomputes each entry_hash. Returns ok=true and n=count
// when the chain is intact; otherwise returns the offending event ID
// and the reason. A chain with NULL prev_hash/entry_hash columns (rows
// inserted before migration 000023) is treated as "unverified" — the
// walk skips them and reports the first verified-vs-recomputed mismatch.
type ChainVerifyResult struct {
	OK              bool   `json:"ok"`
	Count           int    `json:"count"`
	FirstBadEventID string `json:"first_bad_event_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

func (r *EventRepository) VerifyChain() (*ChainVerifyResult, error) {
	rows, err := r.db.Query(`
		SELECT id, type, run_id, step_id, payload, timestamp, prev_hash, entry_hash
		FROM events
		WHERE entry_hash IS NOT NULL
		ORDER BY timestamp ASC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to scan events for verify: %w", err)
	}
	defer rows.Close()

	expectedPrev := ""
	count := 0
	for rows.Next() {
		var (
			id, evtType                string
			runID, stepID              sql.NullString
			payload                    []byte
			ts                         time.Time
			storedPrev, storedEntry    sql.NullString
		)
		if err := rows.Scan(&id, &evtType, &runID, &stepID, &payload, &ts, &storedPrev, &storedEntry); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if storedPrev.String != expectedPrev {
			return &ChainVerifyResult{
				OK:              false,
				Count:           count,
				FirstBadEventID: id,
				Reason:          fmt.Sprintf("prev_hash mismatch: expected %q, found %q", expectedPrev, storedPrev.String),
			}, nil
		}
		evt := &domain.Event{
			ID:        id,
			Type:      domain.EventType(evtType),
			RunID:     runID.String,
			Timestamp: ts,
		}
		if stepID.Valid {
			s := stepID.String
			evt.StepID = &s
		}
		recomputed := computeEntryHash(storedPrev.String, evt, payload)
		if recomputed != storedEntry.String {
			return &ChainVerifyResult{
				OK:              false,
				Count:           count,
				FirstBadEventID: id,
				Reason:          fmt.Sprintf("entry_hash mismatch: recomputed %s, stored %s", recomputed, storedEntry.String),
			}, nil
		}
		expectedPrev = storedEntry.String
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &ChainVerifyResult{OK: true, Count: count}, nil
}

// ListByRun retrieves events for a specific run
func (r *EventRepository) ListByRun(runID string, limit int) ([]*domain.Event, error) {
	query := `
		SELECT id, type, run_id, step_id, payload, timestamp
		FROM events WHERE run_id = ? ORDER BY timestamp DESC LIMIT ?
	`
	rows, err := r.db.Query(query, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list events: %w", err)
	}
	defer rows.Close()

	return r.scanEvents(rows)
}

// ListByType retrieves events by type
func (r *EventRepository) ListByType(eventType domain.EventType, limit int) ([]*domain.Event, error) {
	query := `
		SELECT id, type, run_id, step_id, payload, timestamp
		FROM events WHERE type = ? ORDER BY timestamp DESC LIMIT ?
	`
	rows, err := r.db.Query(query, eventType, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list events: %w", err)
	}
	defer rows.Close()

	return r.scanEvents(rows)
}

// ListAll retrieves all events
func (r *EventRepository) ListAll(limit int) ([]*domain.Event, error) {
	query := `
		SELECT id, type, run_id, step_id, payload, timestamp
		FROM events ORDER BY timestamp DESC LIMIT ?
	`
	rows, err := r.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list events: %w", err)
	}
	defer rows.Close()

	return r.scanEvents(rows)
}

// ListByTimeRange retrieves events in [from, to].
func (r *EventRepository) ListByTimeRange(from, to time.Time) ([]*domain.Event, error) {
	query := `
		SELECT id, type, run_id, step_id, payload, timestamp
		FROM events
		WHERE timestamp >= ? AND timestamp <= ?
		ORDER BY timestamp ASC
	`
	rows, err := r.db.Query(query, from, to)
	if err != nil {
		return nil, fmt.Errorf("failed to list events by time range: %w", err)
	}
	defer rows.Close()

	return r.scanEvents(rows)
}

// DeleteOlderThan removes events older than cutoff and returns deleted rows.
func (r *EventRepository) DeleteOlderThan(cutoff time.Time) (int64, error) {
	res, err := r.db.Exec("DELETE FROM events WHERE timestamp < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to prune events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// scanEvents scans rows into Event structs
func (r *EventRepository) scanEvents(rows *sql.Rows) ([]*domain.Event, error) {
	events := make([]*domain.Event, 0)

	for rows.Next() {
		event := &domain.Event{}
		var runID, stepID sql.NullString
		var payload []byte

		err := rows.Scan(
			&event.ID, &event.Type, &runID, &stepID,
			&payload, &event.Timestamp,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan event: %w", err)
		}

		if runID.Valid {
			event.RunID = runID.String
		}
		if stepID.Valid {
			event.StepID = &stepID.String
		}

		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return events, nil
}

// MemoryRepository handles database operations for Memory
type MemoryRepository struct {
	db *DB
}

// NewMemoryRepository creates a new MemoryRepository
func NewMemoryRepository(db *DB) *MemoryRepository {
	return &MemoryRepository{db: db}
}

// Create inserts a new memory entry
func (r *MemoryRepository) Create(entry *domain.MemoryEntry) error {
	query := `
		INSERT INTO memory (id, scope, content, assistant_id, run_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`
	_, err := r.db.Exec(query,
		entry.ID, entry.Scope, entry.Content,
		entry.AssistantID, entry.RunID, entry.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create memory entry: %w", err)
	}
	return nil
}

// GetByID retrieves a memory entry by ID
func (r *MemoryRepository) GetByID(id string) (*domain.MemoryEntry, error) {
	query := `
		SELECT id, scope, content, assistant_id, run_id, created_at
		FROM memory WHERE id = ?
	`
	entry := &domain.MemoryEntry{}
	var assistantID, runID sql.NullString

	err := r.db.QueryRow(query, id).Scan(
		&entry.ID, &entry.Scope, &entry.Content,
		&assistantID, &runID, &entry.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory entry not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get memory entry: %w", err)
	}

	if assistantID.Valid {
		entry.AssistantID = &assistantID.String
	}
	if runID.Valid {
		entry.RunID = &runID.String
	}

	return entry, nil
}

// ListByScope retrieves memory entries by scope
func (r *MemoryRepository) ListByScope(scope string, limit int) ([]*domain.MemoryEntry, error) {
	query := `
		SELECT id, scope, content, assistant_id, run_id, created_at
		FROM memory WHERE scope = ? ORDER BY created_at DESC LIMIT ?
	`
	rows, err := r.db.Query(query, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list memory: %w", err)
	}
	defer rows.Close()

	return r.scanMemory(rows)
}

// ListByAssistant retrieves memory entries for a specific assistant
func (r *MemoryRepository) ListByAssistant(assistantID string, limit int) ([]*domain.MemoryEntry, error) {
	query := `
		SELECT id, scope, content, assistant_id, run_id, created_at
		FROM memory WHERE assistant_id = ? ORDER BY created_at DESC LIMIT ?
	`
	rows, err := r.db.Query(query, assistantID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list memory: %w", err)
	}
	defer rows.Close()

	return r.scanMemory(rows)
}

// scanMemory scans rows into MemoryEntry structs
func (r *MemoryRepository) scanMemory(rows *sql.Rows) ([]*domain.MemoryEntry, error) {
	entries := make([]*domain.MemoryEntry, 0)

	for rows.Next() {
		entry := &domain.MemoryEntry{}
		var assistantID, runID sql.NullString

		err := rows.Scan(
			&entry.ID, &entry.Scope, &entry.Content,
			&assistantID, &runID, &entry.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory: %w", err)
		}

		if assistantID.Valid {
			entry.AssistantID = &assistantID.String
		}
		if runID.Valid {
			entry.RunID = &runID.String
		}

		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return entries, nil
}

// Delete removes a memory entry
func (r *MemoryRepository) Delete(id string) error {
	_, err := r.db.Exec("DELETE FROM memory WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete memory entry: %w", err)
	}
	return nil
}

// Search searches memory entries by content (simple LIKE search)
func (r *MemoryRepository) Search(query string, limit int) ([]*domain.MemoryEntry, error) {
	sqlQuery := `
		SELECT id, scope, content, assistant_id, run_id, created_at
		FROM memory WHERE content LIKE ? ORDER BY created_at DESC LIMIT ?
	`
	rows, err := r.db.Query(sqlQuery, "%"+query+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search memory: %w", err)
	}
	defer rows.Close()

	return r.scanMemory(rows)
}

// AnonymizeByAssistant nulls the assistant_id of every memory row that
// references the deleted assistant. Mirrors the current ON DELETE SET
// NULL FK behavior so the audit value of the memory content survives a
// delete; the link to the (now-gone) assistant is the only thing
// dropped. Idempotent — re-running on already-anonymized rows is a
// no-op. Called by mnemos.Client.Tombstone in response to
// assistant.deleted events; see ADR 0004 §6.
func (r *MemoryRepository) AnonymizeByAssistant(assistantID string) error {
	_, err := r.db.Exec("UPDATE memory SET assistant_id = NULL WHERE assistant_id = ?", assistantID)
	if err != nil {
		return fmt.Errorf("failed to anonymize memory by assistant: %w", err)
	}
	return nil
}

// AnonymizeByRun nulls the run_id of every memory row that references
// the deleted run. Same semantics as AnonymizeByAssistant.
func (r *MemoryRepository) AnonymizeByRun(runID string) error {
	_, err := r.db.Exec("UPDATE memory SET run_id = NULL WHERE run_id = ?", runID)
	if err != nil {
		return fmt.Errorf("failed to anonymize memory by run: %w", err)
	}
	return nil
}

// AppSettingsRepository handles database operations for application settings
type AppSettingsRepository struct {
	db *DB
}

// NewAppSettingsRepository creates a new AppSettingsRepository
func NewAppSettingsRepository(db *DB) *AppSettingsRepository {
	return &AppSettingsRepository{db}
}

// Get retrieves a setting value by key
func (r *AppSettingsRepository) Get(key string) (string, error) {
	var value string
	err := r.db.QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("setting not found: %s", key)
	}
	if err != nil {
		return "", fmt.Errorf("failed to get setting %s: %w", key, err)
	}
	return value, nil
}

// GetOrDefault retrieves a setting value, returning the default if not found
func (r *AppSettingsRepository) GetOrDefault(key, defaultValue string) string {
	value, err := r.Get(key)
	if err != nil {
		return defaultValue
	}
	return value
}

// Set updates or inserts a setting value
func (r *AppSettingsRepository) Set(key, value string) error {
	_, err := r.db.Exec(`
		INSERT INTO app_settings (key, value, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = CURRENT_TIMESTAMP
	`, key, value)
	if err != nil {
		return fmt.Errorf("failed to set %s: %w", key, err)
	}
	return nil
}

// List retrieves all settings
func (r *AppSettingsRepository) List() (map[string]string, error) {
	rows, err := r.db.Query("SELECT key, value FROM app_settings")
	if err != nil {
		return nil, fmt.Errorf("failed to list settings: %w", err)
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("failed to scan setting: %w", err)
		}
		settings[key] = value
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return settings, nil
}
