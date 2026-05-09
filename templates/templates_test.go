package templates

import (
	"testing"

	"github.com/felixgeelhaar/nomi/internal/permissions"
)

func TestBuiltInTemplatesParseAndValidatePolicies(t *testing.T) {
	templates, err := BuiltIn()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}

	if len(templates) != 8 {
		t.Fatalf("expected 8 templates, got %d", len(templates))
	}

	engine := permissions.NewEngine()
	seenIDs := map[string]struct{}{}
	for _, tpl := range templates {
		if tpl.ID == "" {
			t.Fatal("template missing id")
		}
		if tpl.Name == "" {
			t.Fatalf("template %s missing name", tpl.ID)
		}
		if tpl.TemplateID == "" {
			t.Fatalf("template %s missing template_id", tpl.ID)
		}
		if tpl.Tagline == "" {
			t.Fatalf("template %s missing tagline", tpl.ID)
		}
		if tpl.BestFor == "" {
			t.Fatalf("template %s missing best_for", tpl.ID)
		}
		if tpl.NotFor == "" {
			t.Fatalf("template %s missing not_for", tpl.ID)
		}
		if tpl.SuggestedModel == "" {
			t.Fatalf("template %s missing suggested_model", tpl.ID)
		}
		if !tpl.MemoryPolicy.Enabled {
			t.Fatalf("template %s should enable memory", tpl.ID)
		}
		if tpl.MemoryPolicy.Scope != "workspace" {
			t.Fatalf("template %s expected workspace memory scope, got %q", tpl.ID, tpl.MemoryPolicy.Scope)
		}
		if tpl.MemoryPolicy.SummaryTemplate == "" {
			t.Fatalf("template %s missing memory summary_template", tpl.ID)
		}
		if _, exists := seenIDs[tpl.ID]; exists {
			t.Fatalf("duplicate template id: %s", tpl.ID)
		}
		seenIDs[tpl.ID] = struct{}{}

		if err := engine.ValidatePolicy(tpl.PermissionPolicy); err != nil {
			t.Fatalf("template %s has invalid permission policy: %v", tpl.ID, err)
		}
	}
}
