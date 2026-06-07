package db

import (
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// EmailTriggerRepository handles CRUD + lookup for email_trigger_rules.
// Replaces the JSON-only connection.config["trigger_rules"] approach.
type EmailTriggerRepository struct {
	db *DB
}

// NewEmailTriggerRepository constructs a repository.
func NewEmailTriggerRepository(db *DB) *EmailTriggerRepository {
	return &EmailTriggerRepository{db: db}
}

// Create inserts a new trigger rule.
func (r *EmailTriggerRepository) Create(rule *domain.TriggerRule, connID, name string) error {
	id := fmt.Sprintf("etr-%d", time.Now().UnixNano())
	_, err := r.db.Exec(
		`INSERT INTO email_trigger_rules
		   (id, connection_id, name, assistant_id, from_contains, subject_contains, body_contains, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, connID, name, rule.AssistantID,
		rule.FromContains, rule.SubjectContains, rule.BodyContains,
		rule.Enabled, time.Now().UTC(), time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert email trigger rule: %w", err)
	}
	return nil
}

// ListByConnection returns all rules for a connection, ordered by name.
func (r *EmailTriggerRepository) ListByConnection(connID string) ([]domain.TriggerRule, error) {
	rows, err := r.db.Query(
		`SELECT name, assistant_id, from_contains, subject_contains, body_contains, enabled
		 FROM email_trigger_rules WHERE connection_id = ? ORDER BY name ASC`,
		connID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.TriggerRule, 0)
	for rows.Next() {
		var rule domain.TriggerRule
		if err := rows.Scan(&rule.Name, &rule.AssistantID,
			&rule.FromContains, &rule.SubjectContains, &rule.BodyContains,
			&rule.Enabled); err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// Update patches a trigger rule by name + connection_id.
func (r *EmailTriggerRepository) Update(connID, name string, rule *domain.TriggerRule) error {
	_, err := r.db.Exec(
		`UPDATE email_trigger_rules SET
		   assistant_id = ?, from_contains = ?, subject_contains = ?, body_contains = ?, enabled = ?, updated_at = ?
		 WHERE connection_id = ? AND name = ?`,
		rule.AssistantID, rule.FromContains, rule.SubjectContains,
		rule.BodyContains, rule.Enabled, time.Now().UTC(),
		connID, name,
	)
	if err != nil {
		return fmt.Errorf("update email trigger rule: %w", err)
	}
	return nil
}

// Delete removes a trigger rule by name + connection_id.
func (r *EmailTriggerRepository) Delete(connID, name string) error {
	_, err := r.db.Exec(
		`DELETE FROM email_trigger_rules WHERE connection_id = ? AND name = ?`,
		connID, name,
	)
	if err != nil {
		return fmt.Errorf("delete email trigger rule: %w", err)
	}
	return nil
}
