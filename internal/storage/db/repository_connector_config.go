package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ConnectorConfigRepository handles connector configuration persistence
type ConnectorConfigRepository struct {
	db *DB
}

// NewConnectorConfigRepository creates a new repository
func NewConnectorConfigRepository(db *DB) *ConnectorConfigRepository {
	return &ConnectorConfigRepository{db: db}
}

// DB returns the underlying *DB. Used by the Telegram legacy→plugin
// bridge (api/connectors.go) to construct sibling repositories without
// threading the *DB through every call site. Remove once plugin-ui-01
// lands and the bridge is retired.
func (r *ConnectorConfigRepository) DB() *DB { return r.db }

// ConnectorConfigRecord represents a stored connector configuration
type ConnectorConfigRecord struct {
	ConnectorName string          `json:"connector_name"`
	Config        json.RawMessage `json:"config"`
	Enabled       bool            `json:"enabled"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// Get retrieves a connector config by name
func (r *ConnectorConfigRepository) Get(connectorName string) (*ConnectorConfigRecord, error) {
	query := `SELECT connector_name, config, enabled, updated_at FROM connector_configs WHERE connector_name = ?`
	var record ConnectorConfigRecord
	var configJSON string

	err := r.db.QueryRow(query, connectorName).Scan(&record.ConnectorName, &configJSON, &record.Enabled, &record.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get connector config: %w", err)
	}

	record.Config = json.RawMessage(configJSON)
	return &record, nil
}

// List retrieves all connector configs
func (r *ConnectorConfigRepository) List() ([]*ConnectorConfigRecord, error) {
	query := `SELECT connector_name, config, enabled, updated_at FROM connector_configs ORDER BY connector_name`
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to list connector configs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]*ConnectorConfigRecord, 0)
	for rows.Next() {
		var record ConnectorConfigRecord
		var configJSON string
		if err := rows.Scan(&record.ConnectorName, &configJSON, &record.Enabled, &record.UpdatedAt); err != nil {
			continue
		}
		record.Config = json.RawMessage(configJSON)
		records = append(records, &record)
	}
	return records, rows.Err()
}

// Upsert creates or updates a connector config
func (r *ConnectorConfigRepository) Upsert(connectorName string, config json.RawMessage, enabled bool) error {
	query := `INSERT INTO connector_configs (connector_name, config, enabled, updated_at)
			  VALUES (?, ?, ?, ?)
			  ON CONFLICT(connector_name) DO UPDATE SET
				config = excluded.config,
				enabled = excluded.enabled,
				updated_at = excluded.updated_at`

	_, err := r.db.Exec(query, connectorName, string(config), enabled, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("failed to upsert connector config: %w", err)
	}
	return nil
}

// Delete removes a connector config
func (r *ConnectorConfigRepository) Delete(connectorName string) error {
	_, err := r.db.Exec("DELETE FROM connector_configs WHERE connector_name = ?", connectorName)
	return err
}
