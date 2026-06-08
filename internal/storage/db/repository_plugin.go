package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// ConnectionRepository handles CRUD for plugin_connections.
type ConnectionRepository struct {
	db *DB
}

// NewConnectionRepository wires a new connection repository.
func NewConnectionRepository(db *DB) *ConnectionRepository {
	return &ConnectionRepository{db: db}
}

// Create inserts a new connection. Callers should already have resolved
// credential plaintext → secret:// references before calling this.
func (r *ConnectionRepository) Create(conn *domain.Connection) error {
	if conn.ID == "" {
		return fmt.Errorf("connection id is required")
	}
	if conn.PluginID == "" {
		return fmt.Errorf("connection plugin_id is required")
	}
	configJSON, err := json.Marshal(nonNilMap(conn.Config))
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	credsJSON, err := json.Marshal(nonNilStringMap(conn.CredentialRefs))
	if err != nil {
		return fmt.Errorf("marshal credential_refs: %w", err)
	}
	allowlistJSON, err := json.Marshal(conn.WebhookEventAllowlist)
	if err != nil {
		return fmt.Errorf("marshal webhook_event_allowlist: %w", err)
	}
	if conn.CreatedAt.IsZero() {
		conn.CreatedAt = time.Now().UTC()
	}
	if conn.UpdatedAt.IsZero() {
		conn.UpdatedAt = conn.CreatedAt
	}
	_, err = r.db.Exec(
		`INSERT INTO plugin_connections (id, plugin_id, name, config, credential_refs, enabled, created_at, updated_at, webhook_url, webhook_event_allowlist, webhook_enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conn.ID, conn.PluginID, conn.Name, string(configJSON), string(credsJSON),
		boolToInt(conn.Enabled), conn.CreatedAt, conn.UpdatedAt,
		conn.WebhookURL, string(allowlistJSON), boolToInt(conn.WebhookEnabled),
	)
	if err != nil {
		return fmt.Errorf("insert plugin_connection: %w", err)
	}
	return nil
}

// Update replaces an existing row in place. UpdatedAt is refreshed
// server-side so callers don't need to stamp it. CreatedAt is preserved
// (not overwritten from the caller's struct, which may be zero).
func (r *ConnectionRepository) Update(conn *domain.Connection) error {
	configJSON, err := json.Marshal(nonNilMap(conn.Config))
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	credsJSON, err := json.Marshal(nonNilStringMap(conn.CredentialRefs))
	if err != nil {
		return fmt.Errorf("marshal credential_refs: %w", err)
	}
	allowlistJSON, err := json.Marshal(conn.WebhookEventAllowlist)
	if err != nil {
		return fmt.Errorf("marshal webhook_event_allowlist: %w", err)
	}
	conn.UpdatedAt = time.Now().UTC()
	res, err := r.db.Exec(
		`UPDATE plugin_connections
		 SET plugin_id = ?, name = ?, config = ?, credential_refs = ?, enabled = ?, updated_at = ?, webhook_url = ?, webhook_event_allowlist = ?, webhook_enabled = ?
		 WHERE id = ?`,
		conn.PluginID, conn.Name, string(configJSON), string(credsJSON),
		boolToInt(conn.Enabled), conn.UpdatedAt, conn.WebhookURL, string(allowlistJSON),
		boolToInt(conn.WebhookEnabled), conn.ID,
	)
	if err != nil {
		return fmt.Errorf("update plugin_connection: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("plugin_connection %q not found", conn.ID)
	}
	return nil
}

// GetByID returns one connection or sql.ErrNoRows if not present.
func (r *ConnectionRepository) GetByID(id string) (*domain.Connection, error) {
	row := r.db.QueryRow(
		`SELECT id, plugin_id, name, config, credential_refs, enabled, created_at, updated_at, webhook_url, webhook_event_allowlist, webhook_enabled
		 FROM plugin_connections WHERE id = ?`,
		id,
	)
	return scanConnection(row)
}

// ListByPlugin returns every connection for a given plugin, enabled or not.
// The UI uses this to render the connection list inside a plugin card.
func (r *ConnectionRepository) ListByPlugin(pluginID string) ([]*domain.Connection, error) {
	rows, err := r.db.Query(
		`SELECT id, plugin_id, name, config, credential_refs, enabled, created_at, updated_at, webhook_url, webhook_event_allowlist, webhook_enabled
		 FROM plugin_connections WHERE plugin_id = ? ORDER BY created_at ASC`,
		pluginID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectConnections(rows)
}

// ListEnabled returns every enabled connection across all plugins.
// Used at daemon startup to know which connections a plugin should try
// to activate during Start().
func (r *ConnectionRepository) ListEnabled() ([]*domain.Connection, error) {
	rows, err := r.db.Query(
		`SELECT id, plugin_id, name, config, credential_refs, enabled, created_at, updated_at, webhook_url, webhook_event_allowlist, webhook_enabled
		 FROM plugin_connections WHERE enabled = 1 ORDER BY plugin_id, created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectConnections(rows)
}

// Delete removes a connection. The assistant_connection_bindings junction
// cascades (see migration 10) so we don't have to clean up bindings here.
func (r *ConnectionRepository) Delete(id string) error {
	res, err := r.db.Exec(`DELETE FROM plugin_connections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete plugin_connection: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("plugin_connection %q not found", id)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConnection(row rowScanner) (*domain.Connection, error) {
	var (
		conn           domain.Connection
		cfg            string
		creds          string
		enabled        int
		webhookURL     sql.NullString
		allowlist      string
		webhookEnabled int
	)
	err := row.Scan(&conn.ID, &conn.PluginID, &conn.Name, &cfg, &creds, &enabled, &conn.CreatedAt, &conn.UpdatedAt,
		&webhookURL, &allowlist, &webhookEnabled)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(cfg), &conn.Config); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := json.Unmarshal([]byte(creds), &conn.CredentialRefs); err != nil {
		return nil, fmt.Errorf("unmarshal credential_refs: %w", err)
	}
	conn.Enabled = enabled != 0
	conn.WebhookURL = webhookURL.String
	conn.WebhookEnabled = webhookEnabled != 0
	if allowlist != "" && allowlist != "null" {
		if err := json.Unmarshal([]byte(allowlist), &conn.WebhookEventAllowlist); err != nil {
			return nil, fmt.Errorf("unmarshal webhook_event_allowlist: %w", err)
		}
	}
	return &conn, nil
}

func collectConnections(rows *sql.Rows) ([]*domain.Connection, error) {
	var out []*domain.Connection
	for rows.Next() {
		conn, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// AssistantBindingRepository
// ---------------------------------------------------------------------------

// AssistantBindingRepository handles CRUD for the assistant_connection_bindings
// junction table.
type AssistantBindingRepository struct {
	db *DB
}

// NewAssistantBindingRepository wires a new binding repository.
func NewAssistantBindingRepository(db *DB) *AssistantBindingRepository {
	return &AssistantBindingRepository{db: db}
}

// Upsert inserts or replaces a binding row. Used for the "build my agent"
// composer so each save is idempotent — the UI sends the full desired set,
// the server reconciles. Primary-uniqueness (only one primary per
// (assistant, plugin, role)) is enforced elsewhere because it needs a
// join against plugin_connections.
func (r *AssistantBindingRepository) Upsert(b *domain.AssistantConnectionBinding) error {
	if b.AssistantID == "" || b.ConnectionID == "" {
		return fmt.Errorf("binding requires assistant_id and connection_id")
	}
	if !b.Role.IsValid() {
		return fmt.Errorf("invalid binding role %q", b.Role)
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(
		`INSERT INTO assistant_connection_bindings
		   (assistant_id, connection_id, role, enabled, is_primary, priority, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(assistant_id, connection_id, role) DO UPDATE SET
		   enabled    = excluded.enabled,
		   is_primary = excluded.is_primary,
		   priority   = excluded.priority`,
		b.AssistantID, b.ConnectionID, string(b.Role),
		boolToInt(b.Enabled), boolToInt(b.IsPrimary), b.Priority, b.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert assistant_connection_binding: %w", err)
	}

	// If this binding was marked primary, clear primary from every other
	// binding in the same (assistant, plugin, role) group. SQLite doesn't
	// support multi-table CHECK constraints, so we do it in Go.
	if b.IsPrimary {
		if err := r.clearOtherPrimaries(b); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a binding. Silently succeeds if nothing matches.
func (r *AssistantBindingRepository) Delete(assistantID, connectionID string, role domain.BindingRole) error {
	_, err := r.db.Exec(
		`DELETE FROM assistant_connection_bindings
		 WHERE assistant_id = ? AND connection_id = ? AND role = ?`,
		assistantID, connectionID, string(role),
	)
	if err != nil {
		return fmt.Errorf("delete assistant_connection_binding: %w", err)
	}
	return nil
}

// ListByAssistant returns every binding for an assistant across all roles
// and connections. The assistant edit view uses this to prefill the
// composer.
func (r *AssistantBindingRepository) ListByAssistant(assistantID string) ([]*domain.AssistantConnectionBinding, error) {
	rows, err := r.db.Query(
		`SELECT assistant_id, connection_id, role, enabled, is_primary, priority, created_at
		 FROM assistant_connection_bindings WHERE assistant_id = ?
		 ORDER BY role, priority DESC, created_at ASC`,
		assistantID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectBindings(rows)
}

// ListByConnection returns every binding for a connection. Used to check
// "which assistants would lose this connection if I delete it?" and to
// fan out inbound channel messages.
func (r *AssistantBindingRepository) ListByConnection(connectionID string) ([]*domain.AssistantConnectionBinding, error) {
	rows, err := r.db.Query(
		`SELECT assistant_id, connection_id, role, enabled, is_primary, priority, created_at
		 FROM assistant_connection_bindings WHERE connection_id = ?
		 ORDER BY role, is_primary DESC, priority DESC`,
		connectionID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectBindings(rows)
}

// ResolvePrimary returns the primary (or highest-priority) binding for
// (assistant, plugin, role). Used at tool-call time: if the LLM omits a
// connection_id, we route via the primary binding.
//
// The join goes through plugin_connections to filter by plugin_id. Uses
// enabled = 1 on both sides to ignore bindings the user has temporarily
// disabled.
func (r *AssistantBindingRepository) ResolvePrimary(assistantID, pluginID string, role domain.BindingRole) (*domain.AssistantConnectionBinding, error) {
	row := r.db.QueryRow(
		`SELECT b.assistant_id, b.connection_id, b.role, b.enabled, b.is_primary, b.priority, b.created_at
		 FROM assistant_connection_bindings b
		 JOIN plugin_connections c ON c.id = b.connection_id
		 WHERE b.assistant_id = ?
		   AND c.plugin_id = ?
		   AND b.role = ?
		   AND b.enabled = 1
		   AND c.enabled = 1
		 ORDER BY b.is_primary DESC, b.priority DESC, b.created_at ASC
		 LIMIT 1`,
		assistantID, pluginID, string(role),
	)
	b, err := scanBinding(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

// HasBinding reports whether an assistant is bound to a specific
// connection in a specific role. The runtime uses this as the "hard wall"
// described in ADR 0001 §7: tool calls targeting a connection the
// assistant isn't bound to must fail with connection_not_bound.
func (r *AssistantBindingRepository) HasBinding(assistantID, connectionID string, role domain.BindingRole) (bool, error) {
	var one int
	err := r.db.QueryRow(
		`SELECT 1 FROM assistant_connection_bindings
		 WHERE assistant_id = ? AND connection_id = ? AND role = ? AND enabled = 1`,
		assistantID, connectionID, string(role),
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return one == 1, nil
}

// clearOtherPrimaries enforces "only one primary binding per
// (assistant, plugin, role)" by nulling the primary flag on every other
// binding in the same group. Called after a primary upsert.
func (r *AssistantBindingRepository) clearOtherPrimaries(b *domain.AssistantConnectionBinding) error {
	_, err := r.db.Exec(
		`UPDATE assistant_connection_bindings
		 SET is_primary = 0
		 WHERE assistant_id = ?
		   AND role = ?
		   AND NOT (connection_id = ?)
		   AND connection_id IN (
		     SELECT c2.id FROM plugin_connections c2
		     JOIN plugin_connections c1 ON c1.id = ?
		     WHERE c2.plugin_id = c1.plugin_id
		   )`,
		b.AssistantID, string(b.Role), b.ConnectionID, b.ConnectionID,
	)
	if err != nil {
		return fmt.Errorf("clear other primaries: %w", err)
	}
	return nil
}

func scanBinding(row rowScanner) (*domain.AssistantConnectionBinding, error) {
	var (
		b         domain.AssistantConnectionBinding
		roleStr   string
		enabled   int
		isPrimary int
	)
	err := row.Scan(&b.AssistantID, &b.ConnectionID, &roleStr, &enabled, &isPrimary, &b.Priority, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	b.Role = domain.BindingRole(roleStr)
	b.Enabled = enabled != 0
	b.IsPrimary = isPrimary != 0
	return &b, nil
}

func collectBindings(rows *sql.Rows) ([]*domain.AssistantConnectionBinding, error) {
	var out []*domain.AssistantConnectionBinding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// nonNilMap returns m if it's non-nil, otherwise an empty map so JSON
// encodes as {} rather than null — avoids surprises on scan.
func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nonNilStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
