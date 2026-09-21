package matrix

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
	if !strings.Contains(text, "Reply APPROVE") {
		t.Fatalf("safe-plan CTA missing: %s", text)
	}
}

func TestFormatPlanReviewText_DesktopOmitsApproveCTA(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	text := formatPlanReviewText("Write file", plan)
	if !strings.Contains(text, "desktop app") {
		t.Fatalf("desktop gate missing: %s", text)
	}
	if strings.Contains(text, "Reply APPROVE to execute") {
		t.Fatalf("approve CTA must be omitted for desktop-gated plans: %s", text)
	}
	if !strings.Contains(text, "Reply DENY") {
		t.Fatalf("deny CTA required: %s", text)
	}
}

func TestPlanRequiresDesktopReview_WriteAndPatch(t *testing.T) {
	write := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	if !planRequiresDesktopReview(write) {
		t.Fatal("filesystem.write must require desktop")
	}
	safe := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read",
	}}}
	if planRequiresDesktopReview(safe) {
		t.Fatal("read-only plan must not require desktop")
	}
}

func TestParsePlanReply(t *testing.T) {
	cases := []struct {
		body    string
		approve bool
		ok      bool
	}{
		{"APPROVE", true, true},
		{"approve plan", true, true},
		{"YES", true, true},
		{"DENY", false, true},
		{"no", false, true},
		{"APPROVE\n\nquoted", true, true},
		{"please approve this", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		approve, ok := parsePlanReply(tc.body)
		if approve != tc.approve || ok != tc.ok {
			t.Fatalf("parsePlanReply(%q)=(%v,%v) want (%v,%v)", tc.body, approve, ok, tc.approve, tc.ok)
		}
	}
}

func TestParseReactionKey(t *testing.T) {
	if a, ok := parseReactionKey("✅"); !ok || !a {
		t.Fatal("check mark should approve")
	}
	if a, ok := parseReactionKey("❌"); !ok || a {
		t.Fatal("x should deny")
	}
	if _, ok := parseReactionKey("🔥"); ok {
		t.Fatal("unrelated reaction should be ignored")
	}
}
