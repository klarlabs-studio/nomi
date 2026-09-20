package telegram

import (
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestFormatPlanReviewText_IncludesGoalAndSteps(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{
		{Title: "Read README", ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read", Order: 0},
		{Title: "Summarize", ExpectedTool: "llm.chat", ExpectedCapability: "llm.chat", Order: 1},
	}}
	text := formatPlanReviewText("Explain the repo", plan)
	if !strings.Contains(text, "Explain the repo") {
		t.Fatalf("goal missing: %s", text)
	}
	if !strings.Contains(text, "Read README") || !strings.Contains(text, "filesystem.read") {
		t.Fatalf("step missing: %s", text)
	}
	if !strings.Contains(text, "Approve to execute") {
		t.Fatalf("safe-plan CTA missing: %s", text)
	}
}

func TestPlanRequiresDesktopReview_WriteAndPatch(t *testing.T) {
	write := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	if !planRequiresDesktopReview(write) {
		t.Fatal("filesystem.write must require desktop")
	}
	patch := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.patch", ExpectedCapability: "filesystem.write",
	}}}
	if !planRequiresDesktopReview(patch) {
		t.Fatal("filesystem.patch must require desktop")
	}
	safe := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read",
	}}}
	if planRequiresDesktopReview(safe) {
		t.Fatal("read-only plan must not require desktop")
	}
}

func TestPlanRequiresDesktopReview_MutatingMCP(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "mcp.docs.write_file", ExpectedCapability: "mcp.docs.write_file",
	}}}
	if !planRequiresDesktopReview(plan) {
		t.Fatal("mutating MCP must require desktop")
	}
}

func TestPlanRequiresDesktopReview_IrreversibleShell(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "command.exec", ExpectedCapability: "command.exec",
		Arguments: map[string]interface{}{"command": "rm -rf /tmp/junk"},
	}}}
	if !planRequiresDesktopReview(plan) {
		t.Fatal("rm -rf must require desktop")
	}
	safe := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "command.exec", ExpectedCapability: "command.exec",
		Arguments: map[string]interface{}{"command": "git status"},
	}}}
	if planRequiresDesktopReview(safe) {
		t.Fatal("safe shell should not force desktop")
	}
}
