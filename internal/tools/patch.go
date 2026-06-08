package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// FilePatchTool applies a unified diff to files inside the assistant's
// workspace. The diff is fed to `git apply` so we get battle-tested
// hunk matching and clear error reporting; we do our own path checks
// before invoking git so a malicious diff can't escape the workspace
// via `--- a/../../etc/passwd` and an LLM that hallucinated a path
// gets a precise ErrCodePatchFileMissing instead of a generic apply
// failure.
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

// MaxPatchBytes caps the unified diff size. A runaway LLM emitting
// a multi-megabyte diff would otherwise OOM exec.CombinedOutput; 1 MB
// is well above any realistic edit size.
const MaxPatchBytes = 1 * 1024 * 1024

// patchPathRE matches the `--- a/path` and `+++ b/path` headers in a
// unified diff. Used both for path-allowlist validation and for the
// summary returned to the UI.
var patchPathRE = regexp.MustCompile(`^(?:---|\+\+\+)\s+(?:[ab]/)?(.+?)\s*$`)

// Execute applies the unified diff in input["diff"] inside
// input["workspace_root"]. The flow is:
//
//  1. Size-cap the diff so a runaway LLM cannot OOM the daemon.
//  2. Parse `---`/`+++` header pairs to learn which files are
//     created, modified, or deleted.
//  3. Allowlist-check every non-/dev/null path against the workspace
//     root.
//  4. Pre-flight: a modify/delete patch must reference an existing
//     file; a create patch must NOT collide with an existing file.
//  5. `git apply --check` first; on rejection retry with
//     `git apply -3 --whitespace=fix` before declaring failure.
//  6. On success, apply for real.
//
// Each step returns a distinct UserError code so the planner / replan
// loop can react appropriately (read+retry vs split+retry vs give up).
func (t *FilePatchTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	diff, ok := input["diff"].(string)
	if !ok || strings.TrimSpace(diff) == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Missing diff",
			Message: "Nomi needs a unified diff to apply. The planner may have forgotten to include the `diff` argument.",
		}
	}
	if len(diff) > MaxPatchBytes {
		return nil, &domain.UserError{
			Code:    domain.ErrCodePatchTooLarge,
			Title:   "Patch too large",
			Message: fmt.Sprintf("Patch is %d bytes; the cap is %d. Split the change into smaller patches.", len(diff), MaxPatchBytes),
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

	pairs, added, removed, err := parseDiffPairs(diff)
	if err != nil {
		return nil, err
	}

	files := make([]string, 0, len(pairs))
	for _, p := range pairs {
		// Allowlist + pre-flight every non-/dev/null path. The right
		// path to validate is the side that names a real file: `+++`
		// for creates/modifies, `---` for deletes.
		target := p.NewPath
		if target == "/dev/null" {
			target = p.OldPath
		}
		if target == "/dev/null" || target == "" {
			continue
		}
		resolved, rerr := ResolveWithinRoot(root, target)
		if rerr != nil {
			return nil, &domain.UserError{
				Code:    domain.ErrCodePathOutsideRoot,
				Title:   "Patch touches a file outside the workspace",
				Message: fmt.Sprintf("The patch tries to modify %q, which is outside the attached workspace folder. Nomi only patches files inside the workspace.", target),
			}
		}
		if err := preflightPatchPath(resolved, p); err != nil {
			return nil, err
		}
		files = append(files, target)
	}

	// `git apply --check` is the dry run. If clean, we apply for real.
	// If rejected, retry with -3 + whitespace=fix to recover from
	// whitespace-only mismatches and small drift; only after both
	// retries fail do we surface ErrCodePatchApplyFailed.
	if output, runErr := runGitApply(ctx, root, diff, []string{"apply", "--check", "--whitespace=nowarn", "-"}); runErr != nil {
		// Strict check failed. Try the lenient mode.
		if _, lenientErr := runGitApply(ctx, root, diff, []string{"apply", "-3", "--whitespace=fix", "-"}); lenientErr != nil {
			return nil, &domain.UserError{
				Code:    domain.ErrCodePatchApplyFailed,
				Title:   "Patch did not apply cleanly",
				Message: fmt.Sprintf("git apply rejected the patch: %s", strings.TrimSpace(string(output))),
			}
		}
		// Lenient mode applied successfully on its own; we're done.
		return map[string]interface{}{
			"files":         files,
			"lines_added":   added,
			"lines_removed": removed,
			"applied_via":   "3way_fallback",
			"success":       true,
		}, nil
	}

	if output, runErr := runGitApply(ctx, root, diff, []string{"apply", "--whitespace=nowarn", "-"}); runErr != nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodePatchApplyFailed,
			Title:   "Patch failed to apply after passing dry-run",
			Message: fmt.Sprintf("git apply rejected the patch on the real apply pass: %s", strings.TrimSpace(string(output))),
		}
	}

	return map[string]interface{}{
		"files":         files,
		"lines_added":   added,
		"lines_removed": removed,
		"applied_via":   "strict",
		"success":       true,
	}, nil
}

// runGitApply spawns `git` with the given args, feeding the diff on
// stdin. Returns the combined output so the caller can include it in
// a UserError when something goes wrong.
func runGitApply(ctx context.Context, root, diff string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: fixed git invocation, args built by the patch tool
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(diff)
	cmd.Env = BuildSandboxEnv(nil)
	return cmd.CombinedOutput()
}

// preflightPatchPath enforces the on-disk invariant for each file in
// the patch. A modify/delete must point at an existing file; a create
// must not collide with one. Surfaces ErrCodePatchFileMissing so the
// replan loop can recover by reading the workspace before re-patching.
func preflightPatchPath(absPath string, p diffFilePair) error {
	_, statErr := os.Stat(absPath)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("preflight stat %q: %w", absPath, statErr)
	}

	if p.IsCreate() {
		if exists {
			return &domain.UserError{
				Code:    domain.ErrCodeToolExecution,
				Title:   "Patch creates a file that already exists",
				Message: fmt.Sprintf("The patch's create-block targets %q, but a file already exists there. Read the file first or send a modify-block instead.", filepath.Base(absPath)),
			}
		}
		return nil
	}

	if !exists {
		return &domain.UserError{
			Code:    domain.ErrCodePatchFileMissing,
			Title:   "Patch targets a file that doesn't exist",
			Message: fmt.Sprintf("The patch tries to modify %q, but no such file exists in the workspace. Read the workspace first to find the right path.", filepath.Base(absPath)),
		}
	}
	return nil
}

// diffFilePair captures one `--- ... / +++ ...` header pair.
type diffFilePair struct {
	OldPath string
	NewPath string
}

// IsCreate is true when the diff introduces a new file (old side is
// /dev/null). Used by preflight to forbid colliding with an existing
// path.
func (p diffFilePair) IsCreate() bool { return p.OldPath == "/dev/null" }

// IsDelete is true when the diff removes a file (new side is
// /dev/null).
func (p diffFilePair) IsDelete() bool { return p.NewPath == "/dev/null" }

// parseDiffPairs walks the raw diff and emits one diffFilePair per
// `---`/`+++` header pair, plus the total +/- line counts. Pure
// parsing, no I/O — callers can use it to render a preview before
// deciding to apply.
func parseDiffPairs(diff string) ([]diffFilePair, int, int, error) {
	var pairs []diffFilePair
	added, removed := 0, 0
	var pendingOld string
	for _, line := range strings.Split(diff, "\n") {
		if m := patchPathRE.FindStringSubmatch(line); m != nil {
			if strings.HasPrefix(line, "---") {
				pendingOld = m[1]
				continue
			}
			if strings.HasPrefix(line, "+++") {
				pairs = append(pairs, diffFilePair{OldPath: pendingOld, NewPath: m[1]})
				pendingOld = ""
				continue
			}
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
	if len(pairs) == 0 {
		return nil, 0, 0, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Diff is missing file headers",
			Message: "A unified diff needs `--- a/<path>` and `+++ b/<path>` headers so Nomi knows which files to touch.",
		}
	}
	return pairs, added, removed, nil
}

// SummarizeDiff is the exported preview helper used by the API/UI to
// render a hunk-count preview during plan review. Returns one entry
// per file pair (deduplicated, /dev/null filtered).
func SummarizeDiff(diff string) (files []string, added, removed int, err error) {
	pairs, a, r, perr := parseDiffPairs(diff)
	if perr != nil {
		return nil, 0, 0, perr
	}
	seen := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		target := p.NewPath
		if target == "/dev/null" {
			target = p.OldPath
		}
		if target == "/dev/null" || target == "" || seen[target] {
			continue
		}
		seen[target] = true
		files = append(files, target)
	}
	return files, a, r, nil
}
