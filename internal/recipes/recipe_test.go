package recipes

import (
	"strings"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

func TestParseValidMinimum(t *testing.T) {
	raw := []byte(`
schema_version: 1
id: minimal
name: Minimal
version: 0.1.0
assistant:
  name: Minimal Assistant
  role: tester
  system_prompt: hello
`)
	r, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.ID != "minimal" {
		t.Fatalf("ID: %q", r.ID)
	}
}

func TestParseRejectsUnknownSchemaVersion(t *testing.T) {
	raw := []byte(`
schema_version: 999
id: x
name: x
version: 0.1
assistant:
  name: x
  role: x
  system_prompt: x
`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error for unknown schema_version")
	}
}

func TestParseRejectsMissingFields(t *testing.T) {
	raw := []byte(`
schema_version: 1
id: x
name: x
version: 0.1
assistant:
  name: ""
  role: ""
  system_prompt: ""
`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("expected error for blank assistant fields")
	}
}

func TestHashStableAcrossParseRoundtrip(t *testing.T) {
	r := &Recipe{
		SchemaVersion: 1,
		ID:            "x", Name: "X", Version: "1.0",
		Assistant: AssistantSpec{Name: "X", Role: "r", SystemPrompt: "sp"},
	}
	h1, err := r.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	yaml, err := Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := Parse(yaml)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	h2, err := parsed.Hash()
	if err != nil {
		t.Fatalf("rehash: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash drifted on round-trip: %q vs %q", h1, h2)
	}
}

func TestFromAssistantPicksDefaults(t *testing.T) {
	a := &domain.AssistantDefinition{
		Name:         "My Helper",
		Role:         "general",
		SystemPrompt: "be helpful",
	}
	r, err := FromAssistant("", "", a)
	if err != nil {
		t.Fatalf("FromAssistant: %v", err)
	}
	if r.ID != "my-helper" {
		t.Fatalf("expected slugified ID 'my-helper', got %q", r.ID)
	}
	if r.Version != "0.1.0" {
		t.Fatalf("expected default version 0.1.0, got %q", r.Version)
	}
}

func TestToAssistantDefinition(t *testing.T) {
	r := &Recipe{
		SchemaVersion: 1, ID: "x", Name: "X", Version: "1",
		Assistant: AssistantSpec{
			Name:         "X",
			Role:         "r",
			SystemPrompt: "sp",
			Capabilities: []string{"filesystem.read"},
			ExecutorBackend: "local",
		},
	}
	a := r.ToAssistantDefinition()
	if a.Name != "X" || a.Role != "r" || len(a.Capabilities) != 1 {
		t.Fatalf("unexpected assistant: %+v", a)
	}
}

func TestBuiltInCatalogPresent(t *testing.T) {
	all, err := BuiltInCatalog()
	if err != nil {
		t.Fatalf("BuiltInCatalog: %v", err)
	}
	if len(all) < 3 {
		t.Fatalf("expected at least 3 builtin recipes, got %d", len(all))
	}
	want := []string{"coding-agent", "ops-runbook", "research-assistant"}
	for i, id := range want {
		if all[i].ID != id {
			t.Errorf("builtin[%d].ID: got %q want %q", i, all[i].ID, id)
		}
	}
}

func TestBuiltInByID(t *testing.T) {
	r, err := BuiltInByID("coding-agent")
	if err != nil {
		t.Fatalf("BuiltInByID: %v", err)
	}
	if !strings.Contains(r.Description, "Claude-Code") {
		t.Fatalf("unexpected description: %q", r.Description)
	}
	if _, err := BuiltInByID("nonexistent"); err == nil {
		t.Fatal("expected error for missing recipe")
	}
}
