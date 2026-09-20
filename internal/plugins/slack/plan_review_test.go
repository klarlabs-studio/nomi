package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestBuildPlanReviewBlocks_SafePlanHasApproveAndDeny(t *testing.T) {
	blocks := buildPlanReviewBlocks("Plan ready", "run-1", false)
	rendered, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(rendered)
	if !strings.Contains(s, "nomi_plan_approve:run-1") {
		t.Fatalf("approve action missing: %s", s)
	}
	if !strings.Contains(s, "nomi_plan_deny:run-1") {
		t.Fatalf("deny action missing: %s", s)
	}
}

func TestBuildPlanReviewBlocks_DesktopOnlyOmitsApprove(t *testing.T) {
	blocks := buildPlanReviewBlocks("Plan ready", "run-2", true)
	rendered, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(rendered)
	if strings.Contains(s, "nomi_plan_approve:") {
		t.Fatalf("approve must be omitted for desktop-gated plans: %s", s)
	}
	if !strings.Contains(s, "nomi_plan_deny:run-2") {
		t.Fatalf("deny still required: %s", s)
	}
}

func TestFormatPlanReviewText_FlagsWritePlans(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{{
		Title: "Write file", ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	text := formatPlanReviewText("Ship it", plan)
	if !strings.Contains(text, "desktop app") {
		t.Fatalf("desktop review hint missing: %s", text)
	}
}
