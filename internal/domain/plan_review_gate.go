package domain

import "strings"

// PlanRequiresDesktopReview is true when a plan writes files, patches,
// runs an irreversible shell command, or calls a mutate-shaped MCP tool.
// Channel Approve buttons and safe-plan auto-approve both use this gate
// so DiffPreview stays in the loop for irreversible work.
func PlanRequiresDesktopReview(plan *Plan) bool {
	if plan == nil {
		return false
	}
	for _, s := range plan.Steps {
		cap := s.ExpectedCapability
		tool := s.ExpectedTool
		if cap == "filesystem.write" || tool == "filesystem.write" || tool == "filesystem.patch" {
			return true
		}
		if (cap == "command.exec" || tool == "command.exec") && irreversibleCommand(stepCommandArg(s)) {
			return true
		}
		name := tool
		if name == "" {
			name = cap
		}
		if (strings.HasPrefix(cap, "mcp.") || strings.HasPrefix(tool, "mcp.")) && mutatingToolName(name) {
			return true
		}
	}
	return false
}

func stepCommandArg(s StepDefinition) string {
	if s.Arguments == nil {
		return ""
	}
	if cmd, ok := s.Arguments["command"].(string); ok {
		return cmd
	}
	if cmd, ok := s.Arguments["input"].(string); ok {
		return cmd
	}
	return ""
}

func irreversibleCommand(cmd string) bool {
	lower := strings.ToLower(cmd)
	return strings.Contains(lower, "rm -rf") ||
		strings.HasPrefix(lower, "rm ") ||
		strings.Contains(lower, "mkfs") ||
		strings.Contains(lower, "dd if=")
}

func mutatingToolName(name string) bool {
	n := strings.ToLower(name)
	for _, needle := range []string{
		"write", "delete", "remove", "create", "update", "patch",
		"put", "send", "post", "exec", "run", "destroy", "drop",
		"insert", "mutate",
	} {
		if strings.Contains(n, needle) {
			return true
		}
	}
	return false
}
