package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.klarlabs.de/nomi/internal/domain"
)

// ChannelIdentityRepository manages CRUD for channel_identities (ADR 0001 §9).
type ChannelIdentityRepository struct {
	db *DB
}

// NewChannelIdentityRepository constructs a repository.
func NewChannelIdentityRepository(db *DB) *ChannelIdentityRepository {
	return &ChannelIdentityRepository{db: db}
}

// Create inserts a new identity allowlist entry.
func (r *ChannelIdentityRepository) Create(ident *domain.ChannelIdentity) error {
	if ident.ID == "" {
		ident.ID = uuid.New().String()
	}
	if ident.PluginID == "" || ident.ConnectionID == "" || ident.ExternalIdentifier == "" {
		return fmt.Errorf("plugin_id, connection_id, external_identifier are required")
	}
	allowedJSON, err := json.Marshal(nonNilStringSlice(ident.AllowedAssistants))
	if err != nil {
		return fmt.Errorf("marshal allowed_assistants: %w", err)
	}
	if ident.CreatedAt.IsZero() {
		ident.CreatedAt = time.Now().UTC()
	}
	if ident.UpdatedAt.IsZero() {
		ident.UpdatedAt = ident.CreatedAt
	}
	_, err = r.db.Exec(
		`INSERT INTO channel_identities
		   (id, plugin_id, connection_id, external_identifier, display_name,
		    allowed_assistants, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ident.ID, ident.PluginID, ident.ConnectionID, ident.ExternalIdentifier,
		ident.DisplayName, string(allowedJSON), boolToInt(ident.Enabled),
		ident.CreatedAt, ident.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert channel_identity: %w", err)
	}
	return nil
}

// Update modifies an existing row in place.
func (r *ChannelIdentityRepository) Update(ident *domain.ChannelIdentity) error {
	allowedJSON, err := json.Marshal(nonNilStringSlice(ident.AllowedAssistants))
	if err != nil {
		return err
	}
	ident.UpdatedAt = time.Now().UTC()
	res, err := r.db.Exec(
		`UPDATE channel_identities
		 SET display_name = ?, allowed_assistants = ?, enabled = ?, updated_at = ?
		 WHERE id = ?`,
		ident.DisplayName, string(allowedJSON), boolToInt(ident.Enabled),
		ident.UpdatedAt, ident.ID,
	)
	if err != nil {
		return fmt.Errorf("update channel_identity: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("channel_identity %q not found", ident.ID)
	}
	return nil
}

// FindByExternal is the hot-path lookup called by channel plugins on
// every inbound message. Returns (nil, nil) if absent — the plugin then
// applies the connection's first-contact policy.
func (r *ChannelIdentityRepository) FindByExternal(pluginID, connectionID, externalID string) (*domain.ChannelIdentity, error) {
	row := r.db.QueryRow(
		`SELECT id, plugin_id, connection_id, external_identifier, display_name,
		        allowed_assistants, enabled, created_at, updated_at
		 FROM channel_identities
		 WHERE plugin_id = ? AND connection_id = ? AND external_identifier = ?`,
		pluginID, connectionID, externalID,
	)
	ident, err := scanIdentity(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ident, err
}

// ListByConnection returns every identity on one connection.
func (r *ChannelIdentityRepository) ListByConnection(connectionID string) ([]*domain.ChannelIdentity, error) {
	rows, err := r.db.Query(
		`SELECT id, plugin_id, connection_id, external_identifier, display_name,
		        allowed_assistants, enabled, created_at, updated_at
		 FROM channel_identities WHERE connection_id = ?
		 ORDER BY display_name COLLATE NOCASE ASC`,
		connectionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectIdentities(rows)
}

// Delete removes an identity.
func (r *ChannelIdentityRepository) Delete(id string) error {
	_, err := r.db.Exec(`DELETE FROM channel_identities WHERE id = ?`, id)
	return err
}

// IsAllowed is the channel plugin's canonical check: given an external
// sender on a specific connection, return whether any enabled allowlist
// row matches AND (if assistantID is supplied) whether that assistant is
// in the identity's AllowedAssistants list (empty list = allow all
// assistants that this connection is bound to).
func (r *ChannelIdentityRepository) IsAllowed(pluginID, connectionID, externalID, assistantID string) (bool, error) {
	ident, err := r.FindByExternal(pluginID, connectionID, externalID)
	if err != nil {
		return false, err
	}
	if ident == nil || !ident.Enabled {
		return false, nil
	}
	if assistantID == "" || len(ident.AllowedAssistants) == 0 {
		return true, nil
	}
	for _, a := range ident.AllowedAssistants {
		if a == assistantID {
			return true, nil
		}
	}
	return false, nil
}

func scanIdentity(row rowScanner) (*domain.ChannelIdentity, error) {
	var (
		ident   domain.ChannelIdentity
		allowed string
		enabled int
	)
	err := row.Scan(
		&ident.ID, &ident.PluginID, &ident.ConnectionID, &ident.ExternalIdentifier,
		&ident.DisplayName, &allowed, &enabled,
		&ident.CreatedAt, &ident.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(allowed), &ident.AllowedAssistants); err != nil {
		return nil, fmt.Errorf("unmarshal allowed_assistants: %w", err)
	}
	ident.Enabled = enabled != 0
	return &ident, nil
}

func collectIdentities(rows *sql.Rows) ([]*domain.ChannelIdentity, error) {
	var out []*domain.ChannelIdentity
	for rows.Next() {
		ident, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ident)
	}
	return out, rows.Err()
}

func nonNilStringSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
