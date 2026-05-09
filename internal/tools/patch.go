package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// FilePatchTool applies a unified diff to files inside the assistant's
// workspace. The diff is fed to `git apply` so we get battle-tested
// hunk matching, three-way fallback, and clear error reporting; we do
// our own path-allowlist check before invoking git so a malicious diff
// can't escape the workspace via `--- a/../../etc/passwd`.
//
// Capability is filesystem.write — the same gate as filesystem.write —
// so existing approval rules cover patch operations without operator
// re-permissioning.
type FilePatchTool struct{}

// NewFilePatchTool creates a new FilePatchTool.
func NewFilePatchTool() *FilePatchTool { return &FilePatchTool{} }

// Name returns the tool name.
func (t *FilePatchTool) Name() string { return "filesystem.patch" }

// Capability returns the required capability. Patch shares the
// filesystem.write capability so a single approval rule governs both.
func (t *FilePatchTool) Capability() string { return "filesystem.write" }

// patchPathRE matches the `--- a/path` and `+++ b/path` headers in a
// unified diff. Used both for path-allowlist validation and for the
// summary returned to the UI.
var patchPathRE = regexp.MustCompile(`^(?:---|\+\+\+)\s+(?:[ab]/)?(.+?)\s*$`)

// Execute applies the unified diff in input["diff"] inside
// input["workspace_root"]. Every path mentioned in a `---`/`+++` header
// is resolved against the workspace root before `git apply` runs so a
// malformed diff cannot reach files outside the sandbox.
func (t *FilePatchTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	diff, ok := input["diff"].(string)
	if !ok || strings.TrimSpace(diff) == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Missing diff",
			Message: "Nomi needs a unified diff to apply. The planner may have forgotten to include the `diff` argument.",
		}
	}
	root, err := WorkspaceRootFromInput(input)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeMissingWorkspace,
			Title:   "No workspace folder",
			Message: "This assistant doesn't have a folder context attached. Add one in the assistant builder so Nomi knows where to apply the patch.",
			Action:  "Open Assistant Builder",
		}
	}

	paths, added, removed, err := summarizeDiff(diff)
	if err != nil {
		return nil, err
	}
	for p := range paths {
		// /dev/null appears in headers for new/deleted files; skip the
		// allowlist check for it but still let git apply handle the
		// real target.
		if p == "/dev/null" {
			continue
		}
		if _, rerr := ResolveWithinRoot(root, p); rerr != nil {
			return nil, &domain.UserError{
				Code:    domain.ErrCodePathOutsideRoot,
				Title:   "Patch touches a file outside the workspace",
				Message: fmt.Sprintf("The patch tries to modify %q, which is outside the attached workspace folder. Nomi only patches files inside the workspace.", p),
			}
		}
	}

	cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", "-")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(diff)
	cmd.Env = BuildSandboxEnv(nil)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Patch did not apply cleanly",
			Message: fmt.Sprintf("git apply rejected the patch: %s", strings.TrimSpace(string(output))),
		}
	}

	files := make([]string, 0, len(paths))
	for p := range paths {
		if p == "/dev/null" {
			continue
		}
		files = append(files, p)
	}

	return map[string]interface{}{
		"files":         files,
		"lines_added":   added,
		"lines_removed": removed,
		"success":       true,
	}, nil
}

// summarizeDiff scans the raw diff for the set of touched paths plus
// the total +/- line counts. Pure parsing — no I/O — so callers can use
// it to render a preview before deciding to apply.
func summarizeDiff(diff string) (map[string]struct{}, int, int, error) {
	paths := make(map[string]struct{})
	added, removed := 0, 0
	sawHeader := false
	for _, line := range strings.Split(diff, "\n") {
		if m := patchPathRE.FindStringSubmatch(line); m != nil {
			sawHeader = true
			paths[m[1]] = struct{}{}
			continue
		}
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			added++
		} else if strings.HasPrefix(line, "-") {
			removed++
		}
	}
	if !sawHeader {
		return nil, 0, 0, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Diff is missing file headers",
			Message: "A unified diff needs `--- a/<path>` and `+++ b/<path>` headers so Nomi knows which files to touch.",
		}
	}
	return paths, added, removed, nil
}

// SummarizeDiff is the exported version of summarizeDiff, used by the
// API/UI to render a hunk-count preview during plan review.
func SummarizeDiff(diff string) (files []string, added, removed int, err error) {
	pathSet, a, r, perr := summarizeDiff(diff)
	if perr != nil {
		return nil, 0, 0, perr
	}
	for p := range pathSet {
		if p == "/dev/null" {
			continue
		}
		files = append(files, p)
	}
	return files, a, r, nil
}
