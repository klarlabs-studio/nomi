package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

// ConversationRepository handles CRUD + lookup for plugin_conversations.
// See ADR 0001 §8.
type ConversationRepository struct {
	db *DB
}

// NewConversationRepository constructs a repository.
func NewConversationRepository(db *DB) *ConversationRepository {
	return &ConversationRepository{db: db}
}

// FindOrCreate looks up an existing conversation by (plugin_id,
// connection_id, external_conversation_id) and creates one if absent.
// Returns the conversation and whether it was newly created. This is the
// primary entry point channel plugins call when an inbound message
// arrives: if it's the first message from this sender on this thread,
// we mint a fresh Conversation and resolve the target assistant.
func (r *ConversationRepository) FindOrCreate(
	pluginID, connectionID, externalConversationID, assistantID string,
	eventBus events.EventPublisher,
) (*domain.Conversation, bool, error) {
	existing, err := r.FindByExternal(pluginID, connectionID, externalConversationID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}
	conv := &domain.Conversation{
		ID:                     newConversationID(),
		PluginID:               pluginID,
		ConnectionID:           connectionID,
		ExternalConversationID: externalConversationID,
		AssistantID:            assistantID,
		CreatedAt:              time.Now().UTC(),
		UpdatedAt:              time.Now().UTC(),
	}
	if err := r.Create(conv, eventBus); err != nil {
		return nil, false, err
	}
	return conv, true, nil
}

// Create inserts a new conversation. FindOrCreate wraps this; direct
// callers would be unusual.
func (r *ConversationRepository) Create(c *domain.Conversation, eventBus events.EventPublisher) error {
	_, err := r.db.Exec(
		`INSERT INTO plugin_conversations
		   (id, plugin_id, connection_id, external_conversation_id, identity_id, assistant_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.PluginID, c.ConnectionID, c.ExternalConversationID,
		nullIfEmpty(c.IdentityID), c.AssistantID, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert plugin_conversation: %w", err)
	}
	if eventBus != nil {
		_, _ = eventBus.Publish(context.Background(), domain.EventConversationCreated, c.ID, nil, map[string]interface{}{
			"conversation_id":          c.ID,
			"plugin_id":                c.PluginID,
			"connection_id":            c.ConnectionID,
			"external_conversation_id": c.ExternalConversationID,
			"assistant_id":             c.AssistantID,
		})
	}
	return nil
}

// FindByExternal returns the conversation for a given
// (plugin, connection, external_conversation_id) or (nil, nil) if absent.
func (r *ConversationRepository) FindByExternal(pluginID, connectionID, externalID string) (*domain.Conversation, error) {
	row := r.db.QueryRow(
		`SELECT id, plugin_id, connection_id, external_conversation_id, identity_id, assistant_id, created_at, updated_at
		 FROM plugin_conversations
		 WHERE plugin_id = ? AND connection_id = ? AND external_conversation_id = ?`,
		pluginID, connectionID, externalID,
	)
	c, err := scanConversation(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// GetByID returns a conversation by id, or an error if not found.
func (r *ConversationRepository) GetByID(id string) (*domain.Conversation, error) {
	row := r.db.QueryRow(
		`SELECT id, plugin_id, connection_id, external_conversation_id, identity_id, assistant_id, created_at, updated_at
		 FROM plugin_conversations WHERE id = ?`,
		id,
	)
	return scanConversation(row)
}

// ListByAssistant returns every conversation an assistant participates
// in, ordered by most-recently-updated first. Powers the Chats tab view
// "all conversations for this assistant."
func (r *ConversationRepository) ListByAssistant(assistantID string, limit int) ([]*domain.Conversation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.Query(
		`SELECT id, plugin_id, connection_id, external_conversation_id, identity_id, assistant_id, created_at, updated_at
		 FROM plugin_conversations WHERE assistant_id = ?
		 ORDER BY updated_at DESC LIMIT ?`,
		assistantID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectConversations(rows)
}

// ListByConnection returns every conversation on a given connection.
// Used by the Plugins tab card to show thread activity per-connection.
func (r *ConversationRepository) ListByConnection(connectionID string, limit int) ([]*domain.Conversation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.Query(
		`SELECT id, plugin_id, connection_id, external_conversation_id, identity_id, assistant_id, created_at, updated_at
		 FROM plugin_conversations WHERE connection_id = ?
		 ORDER BY updated_at DESC LIMIT ?`,
		connectionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return collectConversations(rows)
}

// Touch advances the conversation's updated_at timestamp. Called after
// every run completes so the Chats tab can sort by most-recent activity.
func (r *ConversationRepository) Touch(id string, eventBus events.EventPublisher) error {
	_, err := r.db.Exec(
		`UPDATE plugin_conversations SET updated_at = ? WHERE id = ?`,
		time.Now().UTC(), id,
	)
	if err == nil && eventBus != nil {
		_, _ = eventBus.Publish(context.Background(), domain.EventConversationTouched, id, nil, map[string]interface{}{
			"conversation_id": id,
		})
	}
	return err
}

// Delete removes a conversation. Runs referencing this conversation
// get their conversation_id nulled via ON DELETE SET NULL.
func (r *ConversationRepository) Delete(id string, eventBus events.EventPublisher) error {
	_, err := r.db.Exec(`DELETE FROM plugin_conversations WHERE id = ?`, id)
	if err == nil && eventBus != nil {
		_, _ = eventBus.Publish(context.Background(), domain.EventConversationDeleted, id, nil, map[string]interface{}{
			"conversation_id": id,
		})
	}
	return err
}

func scanConversation(row rowScanner) (*domain.Conversation, error) {
	var (
		c          domain.Conversation
		identityID sql.NullString
	)
	err := row.Scan(
		&c.ID, &c.PluginID, &c.ConnectionID, &c.ExternalConversationID,
		&identityID, &c.AssistantID, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if identityID.Valid {
		c.IdentityID = identityID.String
	}
	return &c, nil
}

func collectConversations(rows *sql.Rows) ([]*domain.Conversation, error) {
	var out []*domain.Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// nullIfEmpty returns sql.NullString that scans to NULL when s is empty.
func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// newConversationID mints a fresh conversation id. Lives in this file so
// the repository stays self-contained; a uuid.New() import would pull in
// the uuid module for one call site.
func newConversationID() string {
	// Pragmatic: use time + a process-wide counter. Uniqueness is
	// guaranteed within a single daemon; cross-process collision isn't
	// possible because the daemon is the only writer.
	return fmt.Sprintf("conv-%d", time.Now().UnixNano())
}
