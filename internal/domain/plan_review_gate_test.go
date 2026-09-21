package domain

import "testing"

func TestPlanRequiresDesktopReview(t *testing.T) {
	read := &Plan{Steps: []StepDefinition{{
		ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read",
	}}}
	if PlanRequiresDesktopReview(read) {
		t.Fatal("read-only must not require desktop")
	}
	git := &Plan{Steps: []StepDefinition{{
		ExpectedTool: "command.exec", ExpectedCapability: "command.exec",
		Arguments: map[string]any{"command": "git status"},
	}}}
	if PlanRequiresDesktopReview(git) {
		t.Fatal("git status must not require desktop")
	}
	write := &Plan{Steps: []StepDefinition{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	if !PlanRequiresDesktopReview(write) {
		t.Fatal("write must require desktop")
	}
	patch := &Plan{Steps: []StepDefinition{{ExpectedTool: "filesystem.patch"}}}
	if !PlanRequiresDesktopReview(patch) {
		t.Fatal("patch must require desktop")
	}
	rm := &Plan{Steps: []StepDefinition{{
		ExpectedTool: "command.exec",
		Arguments:    map[string]any{"command": "rm -rf /tmp/x"},
	}}}
	if !PlanRequiresDesktopReview(rm) {
		t.Fatal("rm -rf must require desktop")
	}
	mcp := &Plan{Steps: []StepDefinition{{
		ExpectedTool: "mcp.gh.write_file", ExpectedCapability: "mcp.gh.write_file",
	}}}
	if !PlanRequiresDesktopReview(mcp) {
		t.Fatal("mutating mcp must require desktop")
	}
}
