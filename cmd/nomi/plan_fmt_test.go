package main

import (
	"strings"
	"testing"
)

func TestFormatPlanReview_IncludesGoalStepsAndDiff(t *testing.T) {
	plan := &planPayload{Steps: []planStep{
		{
			Title: "Apply fix", ExpectedTool: "filesystem.patch", ExpectedCapability: "filesystem.write",
			Arguments: map[string]any{
				"diff": "--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n",
			},
		},
		{
			Title: "Read README", ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read",
			Arguments: map[string]any{"path": "README.md"},
		},
	}}
	out := formatPlanReview("Fix the flaky test", plan)
	if !strings.Contains(out, "Fix the flaky test") {
		t.Fatalf("goal missing: %s", out)
	}
	if !strings.Contains(out, "Apply fix") || !strings.Contains(out, "filesystem.write") {
		t.Fatalf("patch step missing: %s", out)
	}
	if !strings.Contains(out, "--- a/foo.go") {
		t.Fatalf("diff body missing: %s", out)
	}
	if !strings.Contains(out, "read: README.md") {
		t.Fatalf("read path missing: %s", out)
	}
	if !strings.Contains(out, "writes files") {
		t.Fatalf("caution banner missing: %s", out)
	}
}

func TestPlanRequiresCaution(t *testing.T) {
	safe := &planPayload{Steps: []planStep{{
		ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read",
	}}}
	if planRequiresCaution(safe) {
		t.Fatal("read-only must not caution")
	}
	write := &planPayload{Steps: []planStep{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	if !planRequiresCaution(write) {
		t.Fatal("write must caution")
	}
	rm := &planPayload{Steps: []planStep{{
		ExpectedTool: "command.exec", ExpectedCapability: "command.exec",
		Arguments: map[string]any{"command": "rm -rf /tmp/x"},
	}}}
	if !planRequiresCaution(rm) {
		t.Fatal("rm -rf must caution")
	}
}

func TestDropPlanSteps(t *testing.T) {
	plan := &planPayload{Steps: []planStep{
		{ID: "a", Title: "One", ExpectedTool: "filesystem.read"},
		{ID: "b", Title: "Two", ExpectedTool: "filesystem.write", DependsOn: []string{"a"}},
		{ID: "c", Title: "Three", ExpectedTool: "filesystem.read", DependsOn: []string{"a", "b"}},
	}}
	edited, err := dropPlanSteps(plan, []int{2})
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Steps) != 2 {
		t.Fatalf("kept %d, want 2", len(edited.Steps))
	}
	if edited.Steps[0].ID != "a" || edited.Steps[1].ID != "c" {
		t.Fatalf("kept ids = %s,%s", edited.Steps[0].ID, edited.Steps[1].ID)
	}
	deps := edited.Steps[1].DependsOn
	if len(deps) != 1 || deps[0] != "a" {
		t.Fatalf("depends_on after drop = %v, want [a]", deps)
	}
	if _, err := dropPlanSteps(plan, []int{1, 2, 3}); err == nil {
		t.Fatal("expected error dropping all steps")
	}
	if _, err := dropPlanSteps(plan, []int{9}); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestEditPlanBodyIncludesArguments(t *testing.T) {
	plan := &planPayload{Steps: []planStep{{
		ID: "s1", Title: "Patch", ExpectedTool: "filesystem.patch",
		Arguments: map[string]any{"diff": "--- a\n+++ b\n"},
	}}}
	body := editPlanBody(plan)
	steps, _ := body["steps"].([]map[string]any)
	if len(steps) != 1 {
		t.Fatalf("steps len = %d", len(steps))
	}
	args, _ := steps[0]["arguments"].(map[string]any)
	if args["diff"] != "--- a\n+++ b\n" {
		t.Fatalf("arguments not preserved: %v", args)
	}
}
