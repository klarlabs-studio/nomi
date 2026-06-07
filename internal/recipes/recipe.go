// Package recipes implements the Recipe registry (roady #125). A Recipe
// is a versioned, shareable bundle of (assistant config, permission
// policy, executor backend pin, future: tool registrations + planner
// prompts). Recipes ship in a built-in catalog and can be exported from
// any existing assistant so users can share their setups.
//
// Wire format: a single recipe.yaml document. Integrity is established
// by an explicit SHA-256 hash over the canonical-serialised content;
// real cryptographic signing (Ed25519) is reserved for a follow-up
// once the catalog moves off-tree.
package recipes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"go.klarlabs.de/nomi/internal/domain"
)

// SchemaVersion is the only schema version recognised today. Bumps
// require backwards-compatible parsing here or a Migration step;
// rejecting unknown versions explicitly beats silent misinterpretation.
const SchemaVersion = 1

// Recipe is the parsed manifest. Field order matches the YAML wire form
// so a canonical serialisation round-trips deterministically.
type Recipe struct {
	SchemaVersion int           `yaml:"schema_version" json:"schema_version"`
	ID            string        `yaml:"id" json:"id"`
	Name          string        `yaml:"name" json:"name"`
	Version       string        `yaml:"version" json:"version"`
	Author        string        `yaml:"author,omitempty" json:"author,omitempty"`
	Description   string        `yaml:"description,omitempty" json:"description,omitempty"`
	Tags          []string      `yaml:"tags,omitempty" json:"tags,omitempty"`
	Assistant     AssistantSpec `yaml:"assistant" json:"assistant"`
}

// AssistantSpec is the subset of AssistantDefinition that a recipe
// captures. Pruned of runtime-only fields (ID, CreatedAt, TemplateID)
// because those are assigned at install time.
type AssistantSpec struct {
	Name             string                  `yaml:"name" json:"name"`
	Tagline          string                  `yaml:"tagline,omitempty" json:"tagline,omitempty"`
	Role             string                  `yaml:"role" json:"role"`
	BestFor          string                  `yaml:"best_for,omitempty" json:"best_for,omitempty"`
	NotFor           string                  `yaml:"not_for,omitempty" json:"not_for,omitempty"`
	SuggestedModel   string                  `yaml:"suggested_model,omitempty" json:"suggested_model,omitempty"`
	SystemPrompt     string                  `yaml:"system_prompt" json:"system_prompt"`
	Capabilities     []string                `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	MemoryPolicy     domain.MemoryPolicy     `yaml:"memory_policy,omitempty" json:"memory_policy,omitempty"`
	PermissionPolicy domain.PermissionPolicy `yaml:"permission_policy,omitempty" json:"permission_policy,omitempty"`
	ExecutorBackend  string                  `yaml:"executor_backend,omitempty" json:"executor_backend,omitempty"`
	SandboxImage     string                  `yaml:"sandbox_image,omitempty" json:"sandbox_image,omitempty"`
}

// Hash returns the canonical SHA-256 hex digest of a recipe. Used to
// pin a recipe version: two installs against the same hex are
// byte-identical bundles. Computed over the YAML re-serialisation so a
// recipe parsed and re-emitted is stable.
func (r *Recipe) Hash() (string, error) {
	buf, err := yaml.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("hash recipe: marshal: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// Validate sanity-checks a parsed recipe. Returns the first violation
// encountered; the caller can surface it to the user without further
// digging.
func (r *Recipe) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", r.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("id is required")
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return errors.New("version is required")
	}
	if strings.TrimSpace(r.Assistant.Name) == "" {
		return errors.New("assistant.name is required")
	}
	if strings.TrimSpace(r.Assistant.Role) == "" {
		return errors.New("assistant.role is required")
	}
	if strings.TrimSpace(r.Assistant.SystemPrompt) == "" {
		return errors.New("assistant.system_prompt is required")
	}
	return nil
}

// Parse loads a recipe from YAML bytes and validates it.
func Parse(raw []byte) (*Recipe, error) {
	var r Recipe
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse recipe: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Marshal canonical-serialises a recipe back to YAML. Used by Export.
func Marshal(r *Recipe) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(r)
}

// FromAssistant derives a Recipe from an existing AssistantDefinition.
// Used by Export so a user can publish their setup. ID slug is derived
// from the assistant name when omitted by the caller.
func FromAssistant(id, version string, a *domain.AssistantDefinition) (*Recipe, error) {
	if a == nil {
		return nil, errors.New("assistant is nil")
	}
	if strings.TrimSpace(id) == "" {
		id = slugify(a.Name)
	}
	if strings.TrimSpace(version) == "" {
		version = "0.1.0"
	}
	r := &Recipe{
		SchemaVersion: SchemaVersion,
		ID:            id,
		Name:          a.Name,
		Version:       version,
		Description:   a.Tagline,
		Assistant: AssistantSpec{
			Name:             a.Name,
			Tagline:          a.Tagline,
			Role:             a.Role,
			BestFor:          a.BestFor,
			NotFor:           a.NotFor,
			SuggestedModel:   a.SuggestedModel,
			SystemPrompt:     a.SystemPrompt,
			Capabilities:     a.Capabilities,
			MemoryPolicy:     a.MemoryPolicy,
			PermissionPolicy: a.PermissionPolicy,
			ExecutorBackend:  a.ExecutorBackend,
			SandboxImage:     a.SandboxImage,
		},
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

// ToAssistantDefinition materialises an AssistantDefinition from a
// Recipe's AssistantSpec. The caller is responsible for setting ID and
// CreatedAt before persisting.
func (r *Recipe) ToAssistantDefinition() *domain.AssistantDefinition {
	a := &domain.AssistantDefinition{
		Name:             r.Assistant.Name,
		Tagline:          r.Assistant.Tagline,
		Role:             r.Assistant.Role,
		BestFor:          r.Assistant.BestFor,
		NotFor:           r.Assistant.NotFor,
		SuggestedModel:   r.Assistant.SuggestedModel,
		SystemPrompt:     r.Assistant.SystemPrompt,
		Capabilities:     r.Assistant.Capabilities,
		MemoryPolicy:     r.Assistant.MemoryPolicy,
		PermissionPolicy: r.Assistant.PermissionPolicy,
		ExecutorBackend:  r.Assistant.ExecutorBackend,
		SandboxImage:     r.Assistant.SandboxImage,
	}
	return a
}

// slugify lowercases, replaces spaces with hyphens, and strips non
// alphanumeric/hyphen runes. Used to derive a recipe ID from a free-
// form assistant name when the caller didn't supply one.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "recipe"
	}
	return out
}
