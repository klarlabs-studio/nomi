package templates

import (
	"testing"

	"go.klarlabs.de/nomi/internal/permissions"
)

// TestBuiltInTemplatesPassCeilingValidation guards every bundled assistant
// definition against the silent-deny trap fixed in V1.3 (see
// internal/permissions/ceiling.go). A new template that ships a non-deny
// policy rule for a capability whose family is not declared would land the
// wizard-completion path in "permission denied" with no actionable error;
// catching that at test time keeps the contract honest as templates evolve.
func TestBuiltInTemplatesPassCeilingValidation(t *testing.T) {
	tpls, err := BuiltIn()
	if err != nil {
		t.Fatalf("BuiltIn() failed: %v", err)
	}
	if len(tpls) == 0 {
		t.Fatal("BuiltIn() returned no templates — embed.FS wiring broken?")
	}
	for _, tpl := range tpls {
		t.Run(tpl.TemplateID, func(t *testing.T) {
			if err := permissions.ValidatePolicyAgainstCeiling(tpl.Capabilities, tpl.PermissionPolicy); err != nil {
				t.Errorf("template %q (%q) has incoherent ceiling/policy: %v",
					tpl.Name, tpl.TemplateID, err)
			}
		})
	}
}
