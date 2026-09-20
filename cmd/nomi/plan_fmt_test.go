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
	var b strings.Builder
	formatPlanReview(&b, "Fix the flaky test", plan)
	out := b.String()
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

func TestFormatPlanReview_CommandExec(t *testing.T) {
	plan := &planPayload{Steps: []planStep{{
		Title: "Run tests", ExpectedTool: "command.exec", ExpectedCapability: "command.exec",
		Arguments: map[string]any{"command": "go test ./..."},
	}}}
	var b strings.Builder
	formatPlanReview(&b, "test", plan)
	if !strings.Contains(b.String(), "$ go test ./...") {
		t.Fatalf("command missing: %s", b.String())
	}
}
