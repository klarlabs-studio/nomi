package templates

import (
	"embed"
	"encoding/json"
	"fmt"

	"go.klarlabs.de/nomi/internal/domain"
)

//go:embed built-in.json
var builtInFS embed.FS

// BuiltIn loads bundled assistant templates from templates/built-in.json.
func BuiltIn() ([]domain.AssistantDefinition, error) {
	raw, err := builtInFS.ReadFile("built-in.json")
	if err != nil {
		return nil, fmt.Errorf("read built-in templates: %w", err)
	}

	var out []domain.AssistantDefinition
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse built-in templates: %w", err)
	}
	return out, nil
}

// ByID returns the bundled template whose template_id matches id.
// Used by the seed loader (and any other caller that wants to
// materialise a template by stable id rather than display name).
func ByID(id string) (*domain.AssistantDefinition, error) {
	all, err := BuiltIn()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].TemplateID == id {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("template %q not found", id)
}
