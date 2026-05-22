package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// ProviderProfileRepository handles provider profile persistence
type ProviderProfileRepository struct {
	db *DB
}

// NewProviderProfileRepository creates a new repository
func NewProviderProfileRepository(db *DB) *ProviderProfileRepository {
	return &ProviderProfileRepository{db: db}
}

// Create inserts a new provider profile
func (r *ProviderProfileRepository) Create(profile *domain.ProviderProfile) error {
	modelIDs, err := json.Marshal(profile.ModelIDs)
	if err != nil {
		return fmt.Errorf("failed to marshal model_ids: %w", err)
	}

	query := `INSERT INTO provider_profiles (id, name, type, endpoint, model_ids, embedding_model_id, secret_ref, enabled, created_at, updated_at)
			  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = r.db.Exec(query, profile.ID, profile.Name, profile.Type, profile.Endpoint,
		string(modelIDs), profile.EmbeddingModelID, profile.SecretRef, profile.Enabled, profile.CreatedAt, profile.UpdatedAt)
	return err
}

// GetByID retrieves a provider profile by ID
func (r *ProviderProfileRepository) GetByID(id string) (*domain.ProviderProfile, error) {
	query := `SELECT id, name, type, endpoint, model_ids, embedding_model_id, secret_ref, enabled, created_at, updated_at
			  FROM provider_profiles WHERE id = ?`
	return r.scanRow(r.db.QueryRow(query, id))
}

// List returns all provider profiles
func (r *ProviderProfileRepository) List() ([]*domain.ProviderProfile, error) {
	query := `SELECT id, name, type, endpoint, model_ids, embedding_model_id, secret_ref, enabled, created_at, updated_at
			  FROM provider_profiles ORDER BY created_at DESC`
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	profiles := make([]*domain.ProviderProfile, 0)
	for rows.Next() {
		profile, err := r.scanRow(rows)
		if err != nil {
			continue
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

// Update updates a provider profile
func (r *ProviderProfileRepository) Update(profile *domain.ProviderProfile) error {
	modelIDs, err := json.Marshal(profile.ModelIDs)
	if err != nil {
		return fmt.Errorf("failed to marshal model_ids: %w", err)
	}

	profile.UpdatedAt = time.Now().UTC()
	query := `UPDATE provider_profiles SET name = ?, type = ?, endpoint = ?, model_ids = ?, embedding_model_id = ?, secret_ref = ?, enabled = ?, updated_at = ?
			  WHERE id = ?`
	_, err = r.db.Exec(query, profile.Name, profile.Type, profile.Endpoint,
		string(modelIDs), profile.EmbeddingModelID, profile.SecretRef, profile.Enabled, profile.UpdatedAt, profile.ID)
	return err
}

// Delete removes a provider profile
func (r *ProviderProfileRepository) Delete(id string) error {
	_, err := r.db.Exec("DELETE FROM provider_profiles WHERE id = ?", id)
	return err
}

func (r *ProviderProfileRepository) scanRow(scanner interface{ Scan(dest ...interface{}) error }) (*domain.ProviderProfile, error) {
	var profile domain.ProviderProfile
	var modelIDsJSON string

	err := scanner.Scan(&profile.ID, &profile.Name, &profile.Type, &profile.Endpoint,
		&modelIDsJSON, &profile.EmbeddingModelID, &profile.SecretRef, &profile.Enabled, &profile.CreatedAt, &profile.UpdatedAt)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(modelIDsJSON), &profile.ModelIDs); err != nil {
		profile.ModelIDs = []string{}
	}

	return &profile, nil
}

// GlobalSettingsRepository handles global app settings
type GlobalSettingsRepository struct {
	db *DB
}

// NewGlobalSettingsRepository creates a new repository
func NewGlobalSettingsRepository(db *DB) *GlobalSettingsRepository {
	return &GlobalSettingsRepository{db: db}
}

// Get retrieves a setting value by key
func (r *GlobalSettingsRepository) Get(key string) (string, error) {
	var value string
	err := r.db.QueryRow("SELECT value FROM global_settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// Set updates or inserts a setting value
func (r *GlobalSettingsRepository) Set(key, value string) error {
	query := `INSERT INTO global_settings (key, value, updated_at) VALUES (?, ?, ?)
			  ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
	_, err := r.db.Exec(query, key, value, time.Now().UTC())
	return err
}

// GetLLMDefault returns the default LLM provider and model IDs
func (r *GlobalSettingsRepository) GetLLMDefault() (providerID, modelID string) {
	providerID, _ = r.Get("llm.default_provider_id")
	modelID, _ = r.Get("llm.default_model_id")
	return
}

// SetLLMDefault sets the default LLM provider and model IDs
func (r *GlobalSettingsRepository) SetLLMDefault(providerID, modelID string) error {
	if err := r.Set("llm.default_provider_id", providerID); err != nil {
		return err
	}
	return r.Set("llm.default_model_id", modelID)
}
