package main

import (
	"fmt"
	"io"
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
	Order              int            `json:"order"`
}

type planPayload struct {
	ID      string     `json:"id"`
	Version int        `json:"version"`
	Steps   []planStep `json:"steps"`
}

// formatPlanReview writes a human-readable plan to w (typically stderr).
// Mirrors the channel plugins' layout, plus argument/diff detail so the
// CLI can do Claude Code-style review without the desktop DiffPreview.
func formatPlanReview(w io.Writer, goal string, plan *planPayload) {
	if plan == nil {
		fmt.Fprintln(w, "▶ plan ready (empty)")
		return
	}
	fmt.Fprintln(w, "▶ Plan ready for review")
	if goal != "" {
		fmt.Fprintf(w, "  Goal: %s\n", truncateRunes(goal, 200))
	}
	fmt.Fprintln(w)

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
		fmt.Fprintf(w, "  %d. %s", i+1, truncateRunes(title, 100))
		if cap != "" {
			fmt.Fprintf(w, " — `%s`", cap)
		}
		fmt.Fprintln(w)
		if s.Description != "" {
			fmt.Fprintf(w, "     %s\n", truncateRunes(s.Description, 200))
		}
		if s.Why != "" {
			fmt.Fprintf(w, "     why: %s\n", truncateRunes(s.Why, 160))
		}
		writeStepArguments(w, s)
	}
	if len(steps) > maxPlanStepsPrinted {
		fmt.Fprintf(w, "  (+%d more)\n", len(steps)-maxPlanStepsPrinted)
	}
	if planRequiresCaution(plan) {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  ⚠ This plan writes files or runs shell/mutating tools — review carefully.")
	}
}

func writeStepArguments(w io.Writer, s planStep) {
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
			fmt.Fprintf(w, "     diff: +%d −%d", added, removed)
			if len(files) > 0 {
				fmt.Fprintf(w, " in %s", strings.Join(files, ", "))
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "     ---")
		fmt.Fprintln(w, indentBlock(truncateRunes(diff, maxDiffPrintRunes), "     "))
		fmt.Fprintln(w, "     ---")
	case "filesystem.write":
		path, _ := s.Arguments["path"].(string)
		content, _ := s.Arguments["content"].(string)
		if path != "" {
			fmt.Fprintf(w, "     write: %s\n", path)
		}
		if content != "" {
			fmt.Fprintln(w, indentBlock(truncateLines(content, maxWriteContentLines), "     | "))
		}
	case "filesystem.read":
		if path, _ := s.Arguments["path"].(string); path != "" {
			fmt.Fprintf(w, "     read: %s\n", path)
		}
	case "command.exec":
		cmd := stepCommand(s)
		if cmd != "" {
			fmt.Fprintf(w, "     $ %s\n", truncateRunes(cmd, 200))
		}
	default:
		// MCP / unknown — show a compact key list, not full payloads.
		keys := make([]string, 0, len(s.Arguments))
		for k := range s.Arguments {
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			fmt.Fprintf(w, "     args: %s\n", strings.Join(keys, ", "))
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
