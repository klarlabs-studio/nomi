package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// RemoteTemplateRepository handles CRUD for remote_templates.
// Mirrors the plugin store schema (provenance + materialized assistant fields).
type RemoteTemplateRepository struct {
	db *DB
}

// NewRemoteTemplateRepository constructs a new repository.
func NewRemoteTemplateRepository(db *DB) *RemoteTemplateRepository {
	return &RemoteTemplateRepository{db: db}
}

// RemoteTemplate represents a row in remote_templates.
type RemoteTemplate struct {
	ID                  string
	CatalogHash         string
	SourceURL           string
	Signature           string
	Name                string
	Tagline             string
	Role                string
	BestFor             string
	NotFor              string
	SuggestedModel      string
	SystemPrompt        string
	Channels            string // JSON array
	Capabilities        string // JSON array
	Contexts            string // JSON array
	MemoryPolicy        string // JSON object
	PermissionPolicy    string // JSON object
	RecommendedBindings string // JSON array
	InstalledAt         string
	LocalAssistantID    string
}

// Install creates or updates a remote template row and materializes a local draft Assistant.
// Returns the new assistant ID.
func (r *RemoteTemplateRepository) Install(rt *RemoteTemplate, assistantID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(`
		INSERT OR REPLACE INTO remote_templates
			(id, catalog_hash, source_url, signature, name, tagline, role,
			 best_for, not_for, suggested_model, system_prompt, channels,
			 capabilities, contexts, memory_policy, permission_policy,
			 recommended_bindings, installed_at, local_assistant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rt.ID, rt.CatalogHash, rt.SourceURL, rt.Signature, rt.Name,
		rt.Tagline, rt.Role, rt.BestFor, rt.NotFor, rt.SuggestedModel,
		rt.SystemPrompt, rt.Channels, rt.Capabilities, rt.Contexts,
		rt.MemoryPolicy, rt.PermissionPolicy, rt.RecommendedBindings,
		now, assistantID,
	)
	if err != nil {
		return fmt.Errorf("install remote template: %w", err)
	}
	return nil
}

// GetByID returns a single remote template by ID.
func (r *RemoteTemplateRepository) GetByID(id string) (*RemoteTemplate, error) {
	row := r.db.QueryRow(`
		SELECT id, catalog_hash, source_url, signature, name, tagline, role,
		       best_for, not_for, suggested_model, system_prompt, channels,
		       capabilities, contexts, memory_policy, permission_policy,
		       recommended_bindings, installed_at, local_assistant_id
		FROM remote_templates WHERE id = ?`, id)
	rt := &RemoteTemplate{}
	err := row.Scan(&rt.ID, &rt.CatalogHash, &rt.SourceURL, &rt.Signature,
		&rt.Name, &rt.Tagline, &rt.Role, &rt.BestFor, &rt.NotFor,
		&rt.SuggestedModel, &rt.SystemPrompt, &rt.Channels, &rt.Capabilities,
		&rt.Contexts, &rt.MemoryPolicy, &rt.PermissionPolicy,
		&rt.RecommendedBindings, &rt.InstalledAt, &rt.LocalAssistantID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get remote template: %w", err)
	}
	return rt, nil
}

// ListAll returns all installed remote templates.
func (r *RemoteTemplateRepository) ListAll() ([]*RemoteTemplate, error) {
	rows, err := r.db.Query(`
		SELECT id, catalog_hash, source_url, signature, name, tagline, role,
		       best_for, not_for, suggested_model, system_prompt, channels,
		       capabilities, contexts, memory_policy, permission_policy,
		       recommended_bindings, installed_at, local_assistant_id
		FROM remote_templates ORDER BY installed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list remote templates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*RemoteTemplate
	for rows.Next() {
		rt := &RemoteTemplate{}
		if err := rows.Scan(&rt.ID, &rt.CatalogHash, &rt.SourceURL, &rt.Signature,
			&rt.Name, &rt.Tagline, &rt.Role, &rt.BestFor, &rt.NotFor,
			&rt.SuggestedModel, &rt.SystemPrompt, &rt.Channels, &rt.Capabilities,
			&rt.Contexts, &rt.MemoryPolicy, &rt.PermissionPolicy,
			&rt.RecommendedBindings, &rt.InstalledAt, &rt.LocalAssistantID); err != nil {
			return nil, fmt.Errorf("scan remote template: %w", err)
		}
		out = append(out, rt)
	}
	return out, rows.Err()
}

// MaterializeAssistant converts a RemoteTemplate to a domain.AssistantDefinition (draft, not active).
func (rt *RemoteTemplate) MaterializeAssistant() (*domain.AssistantDefinition, error) {
	assist := &domain.AssistantDefinition{
		ID:        rt.LocalAssistantID,
		Name:      rt.Name,
		Tagline:   rt.Tagline,
		Role:      rt.Role,
		BestFor:   rt.BestFor,
		NotFor:    rt.NotFor,
		CreatedAt: time.Now(), // Use current time since InstalledAt is a string
	}
	if rt.SuggestedModel != "" {
		assist.SuggestedModel = rt.SuggestedModel
	}
	// Parse JSON fields
	if err := json.Unmarshal([]byte(rt.Channels), &assist.Channels); err != nil && rt.Channels != "" {
		return nil, fmt.Errorf("parse channels: %w", err)
	}
	if err := json.Unmarshal([]byte(rt.Capabilities), &assist.Capabilities); err != nil && rt.Capabilities != "" {
		return nil, fmt.Errorf("parse capabilities: %w", err)
	}
	if err := json.Unmarshal([]byte(rt.Contexts), &assist.Contexts); err != nil && rt.Contexts != "" {
		return nil, fmt.Errorf("parse contexts: %w", err)
	}
	if rt.MemoryPolicy != "" {
		if err := json.Unmarshal([]byte(rt.MemoryPolicy), &assist.MemoryPolicy); err != nil {
			return nil, fmt.Errorf("parse memory_policy: %w", err)
		}
	}
	if rt.PermissionPolicy != "" {
		if err := json.Unmarshal([]byte(rt.PermissionPolicy), &assist.PermissionPolicy); err != nil {
			return nil, fmt.Errorf("parse permission_policy: %w", err)
		}
	}
	if rt.RecommendedBindings != "" {
		if err := json.Unmarshal([]byte(rt.RecommendedBindings), &assist.RecommendedBindings); err != nil {
			return nil, fmt.Errorf("parse recommended_bindings: %w", err)
		}
	}
	if rt.SystemPrompt != "" {
		assist.SystemPrompt = rt.SystemPrompt
	}
	return assist, nil
}
