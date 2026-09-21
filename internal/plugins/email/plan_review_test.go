package email

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
	text := formatPlanReviewText("Explain the repo\n\nFrom: a@b.com\n\nbody", plan)
	if !strings.Contains(text, "Explain the repo") {
		t.Fatalf("goal missing: %s", text)
	}
	if !strings.Contains(text, "Read README") || !strings.Contains(text, "filesystem.read") {
		t.Fatalf("step missing: %s", text)
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

func TestParsePlanReply(t *testing.T) {
	cases := []struct {
		body    string
		approve bool
		ok      bool
	}{
		{"APPROVE", true, true},
		{"approve", true, true},
		{"  Yes  ", true, true},
		{"DENY", false, true},
		{"deny plan", false, true},
		{"CANCEL", false, true},
		{"APPROVE\n\nOn Mon, someone wrote…", true, true},
		{"Please approve this", false, false},
		{"hello", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		approve, ok := parsePlanReply(tc.body)
		if approve != tc.approve || ok != tc.ok {
			t.Fatalf("parsePlanReply(%q)=(%v,%v) want (%v,%v)", tc.body, approve, ok, tc.approve, tc.ok)
		}
	}
}

func TestExtractFromGoal(t *testing.T) {
	goal := "Fix the bug\n\nFrom: alice@example.com\n\nPlease look at this."
	if got := extractFromGoal(goal); got != "alice@example.com" {
		t.Fatalf("got %q", got)
	}
	if got := extractFromGoal("no from line"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestPlanReviewSubject(t *testing.T) {
	if got := planReviewSubject("Hello\n\nFrom: a@b.c\n\nx"); got != "Re: Hello" {
		t.Fatalf("got %q", got)
	}
	if got := planReviewSubject("Re: Already\n\nFrom: a@b.c"); got != "Re: Already" {
		t.Fatalf("got %q", got)
	}
}
