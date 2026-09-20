package main

import (
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/tools"
)

const maxPlanStepsPrinted = 12
const maxWriteContentLines = 20
const maxDiffPrintRunes = 8000

// planStep is the subset of domain.StepDefinition the CLI needs to
// render a plan-review prompt. Kept local so cmd/nomi stays a thin
// HTTP client without importing the full domain package for this path.
type planStep struct {
	ID                 string         `json:"id"`
	Title              string         `json:"title"`
	Description        string         `json:"description"`
	ExpectedTool       string         `json:"expected_tool"`
	ExpectedCapability string         `json:"expected_capability"`
	Why                string         `json:"why"`
	Arguments          map[string]any `json:"arguments"`
	DependsOn          []string       `json:"depends_on"`
	Order              int            `json:"order"`
}

type planPayload struct {
	ID      string     `json:"id"`
	Version int        `json:"version"`
	Steps   []planStep `json:"steps"`
}

// formatPlanReview returns a human-readable plan for stderr.
// Mirrors the channel plugins' layout, plus argument/diff detail so the
// CLI can do Claude Code-style review without the desktop DiffPreview.
func formatPlanReview(goal string, plan *planPayload) string {
	var b strings.Builder
	if plan == nil {
		b.WriteString("▶ plan ready (empty)\n")
		return b.String()
	}
	b.WriteString("▶ Plan ready for review\n")
	if goal != "" {
		fmt.Fprintf(&b, "  Goal: %s\n", truncateRunes(goal, 200))
	}
	b.WriteByte('\n')

	steps := plan.Steps
	limit := maxPlanStepsPrinted
	if len(steps) < limit {
		limit = len(steps)
	}
	for i := 0; i < limit; i++ {
		s := steps[i]
		title := s.Title
		if title == "" {
			title = s.ExpectedTool
		}
		if title == "" {
			title = "step"
		}
		cap := s.ExpectedCapability
		if cap == "" {
			cap = s.ExpectedTool
		}
		fmt.Fprintf(&b, "  %d. %s", i+1, truncateRunes(title, 100))
		if cap != "" {
			fmt.Fprintf(&b, " — `%s`", cap)
		}
		b.WriteByte('\n')
		if s.Description != "" {
			fmt.Fprintf(&b, "     %s\n", truncateRunes(s.Description, 200))
		}
		if s.Why != "" {
			fmt.Fprintf(&b, "     why: %s\n", truncateRunes(s.Why, 160))
		}
		writeStepArguments(&b, s)
	}
	if len(steps) > maxPlanStepsPrinted {
		fmt.Fprintf(&b, "  (+%d more)\n", len(steps)-maxPlanStepsPrinted)
	}
	if planRequiresCaution(plan) {
		b.WriteByte('\n')
		b.WriteString("  ⚠ This plan writes files or runs shell/mutating tools — review carefully.\n")
	}
	return b.String()
}

func writeStepArguments(b *strings.Builder, s planStep) {
	if s.Arguments == nil {
		return
	}
	tool := s.ExpectedTool
	switch tool {
	case "filesystem.patch":
		diff, _ := s.Arguments["diff"].(string)
		if diff == "" {
			return
		}
		files, added, removed, err := tools.SummarizeDiff(diff)
		if err == nil {
			fmt.Fprintf(b, "     diff: +%d −%d", added, removed)
			if len(files) > 0 {
				fmt.Fprintf(b, " in %s", strings.Join(files, ", "))
			}
			b.WriteByte('\n')
		}
		b.WriteString("     ---\n")
		b.WriteString(indentBlock(truncateRunes(diff, maxDiffPrintRunes), "     "))
		b.WriteByte('\n')
		b.WriteString("     ---\n")
	case "filesystem.write":
		path, _ := s.Arguments["path"].(string)
		content, _ := s.Arguments["content"].(string)
		if path != "" {
			fmt.Fprintf(b, "     write: %s\n", path)
		}
		if content != "" {
			b.WriteString(indentBlock(truncateLines(content, maxWriteContentLines), "     | "))
			b.WriteByte('\n')
		}
	case "filesystem.read":
		if path, _ := s.Arguments["path"].(string); path != "" {
			fmt.Fprintf(b, "     read: %s\n", path)
		}
	case "command.exec":
		cmd := stepCommand(s)
		if cmd != "" {
			fmt.Fprintf(b, "     $ %s\n", truncateRunes(cmd, 200))
		}
	default:
		// MCP / unknown — show a compact key list, not full payloads.
		keys := make([]string, 0, len(s.Arguments))
		for k := range s.Arguments {
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			fmt.Fprintf(b, "     args: %s\n", strings.Join(keys, ", "))
		}
	}
}

// planRequiresCaution mirrors channel desktop-gating: write/patch,
// irreversible shell, mutate-shaped MCP. Used only for the warning
// banner — CLI --review always shows the full plan (including diffs).
func planRequiresCaution(plan *planPayload) bool {
	if plan == nil {
		return false
	}
	for _, s := range plan.Steps {
		cap := s.ExpectedCapability
		tool := s.ExpectedTool
		if cap == "filesystem.write" || tool == "filesystem.write" || tool == "filesystem.patch" {
			return true
		}
		if (cap == "command.exec" || tool == "command.exec") && isIrreversibleCommand(stepCommand(s)) {
			return true
		}
		name := tool
		if name == "" {
			name = cap
		}
		if (strings.HasPrefix(cap, "mcp.") || strings.HasPrefix(tool, "mcp.")) && isMutatingToolName(name) {
			return true
		}
	}
	return false
}

func stepCommand(s planStep) string {
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

func isIrreversibleCommand(cmd string) bool {
	lower := strings.ToLower(cmd)
	return strings.Contains(lower, "rm -rf") ||
		strings.HasPrefix(lower, "rm ") ||
		strings.Contains(lower, "mkfs") ||
		strings.Contains(lower, "dd if=")
}

func isMutatingToolName(name string) bool {
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

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func truncateLines(s string, max int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n") + "\n…"
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// dropPlanSteps returns a copy of plan with 1-based step numbers removed.
// DependsOn edges pointing at dropped steps are stripped. Returns an
// error if indices are out of range, empty, or would drop every step.
func dropPlanSteps(plan *planPayload, oneBased []int) (*planPayload, error) {
	if plan == nil || len(plan.Steps) == 0 {
		return nil, fmt.Errorf("plan has no steps to edit")
	}
	if len(oneBased) == 0 {
		return nil, fmt.Errorf("no step numbers given")
	}
	drop := make(map[int]bool, len(oneBased))
	for _, n := range oneBased {
		if n < 1 || n > len(plan.Steps) {
			return nil, fmt.Errorf("step %d out of range (1–%d)", n, len(plan.Steps))
		}
		drop[n-1] = true
	}
	if len(drop) >= len(plan.Steps) {
		return nil, fmt.Errorf("cannot drop every step — deny the plan instead")
	}
	droppedIDs := make(map[string]bool)
	for i, s := range plan.Steps {
		if drop[i] && s.ID != "" {
			droppedIDs[s.ID] = true
		}
	}
	kept := make([]planStep, 0, len(plan.Steps)-len(drop))
	for i, s := range plan.Steps {
		if drop[i] {
			continue
		}
		cp := s
		if len(s.DependsOn) > 0 {
			deps := make([]string, 0, len(s.DependsOn))
			for _, d := range s.DependsOn {
				if !droppedIDs[d] {
					deps = append(deps, d)
				}
			}
			cp.DependsOn = deps
		}
		kept = append(kept, cp)
	}
	return &planPayload{ID: plan.ID, Version: plan.Version, Steps: kept}, nil
}

// editPlanBody is the JSON body for POST /runs/:id/plan/edit.
func editPlanBody(plan *planPayload) map[string]any {
	steps := make([]map[string]any, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		m := map[string]any{
			"title": s.Title,
		}
		if s.ID != "" {
			m["id"] = s.ID
		}
		if s.Description != "" {
			m["description"] = s.Description
		}
		if s.ExpectedTool != "" {
			m["expected_tool"] = s.ExpectedTool
		}
		if s.ExpectedCapability != "" {
			m["expected_capability"] = s.ExpectedCapability
		}
		if len(s.DependsOn) > 0 {
			m["depends_on"] = s.DependsOn
		}
		if len(s.Arguments) > 0 {
			m["arguments"] = s.Arguments
		}
		steps = append(steps, m)
	}
	return map[string]any{"steps": steps}
}
