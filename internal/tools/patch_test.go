package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

// requireGit skips a test when git is unavailable on the host. The patch
// tool delegates hunk application to `git apply`, so without git the
// tool's primary code path can't run; we still cover schema-level
// failures (missing diff, bad path) below in tests that don't need git.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// TestFilePatchTool_AppliesUnifiedDiff covers the happy path: a real
// file, a real diff, a real workspace root, and a real git apply call.
// This is the contract examples/coding-agent depends on.
func TestFilePatchTool_AppliesUnifiedDiff(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	target := filepath.Join(root, "hello.txt")
	if err := os.WriteFile(target, []byte("hi\nthere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := `--- a/hello.txt
+++ b/hello.txt
@@ -1,2 +1,2 @@
-hi
+hello
 there
`
	out, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
		"diff":           diff,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := out["success"].(bool); !got {
		t.Fatalf("expected success=true, got %+v", out)
	}
	body, _ := os.ReadFile(target)
	if string(body) != "hello\nthere\n" {
		t.Fatalf("file body after patch = %q, want %q", body, "hello\nthere\n")
	}
}

// TestFilePatchTool_RefusesPathOutsideWorkspace covers the security
// gate: the diff names a file with `../`, the tool refuses without
// shelling out so a malicious diff cannot reach the host filesystem.
func TestFilePatchTool_RefusesPathOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	diff := `--- a/../escape.txt
+++ b/../escape.txt
@@ -0,0 +1 @@
+pwned
`
	_, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
		"diff":           diff,
	})
	var ue *domain.UserError
	if !errors.As(err, &ue) || ue.Code != domain.ErrCodePathOutsideRoot {
		t.Fatalf("expected ErrCodePathOutsideRoot, got %v", err)
	}
}

// TestFilePatchTool_RejectsMissingDiff confirms the schema-level
// validation matches the planner argument schema's required:["diff"].
func TestFilePatchTool_RejectsMissingDiff(t *testing.T) {
	root := t.TempDir()
	_, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
	})
	var ue *domain.UserError
	if !errors.As(err, &ue) || ue.Code != domain.ErrCodeToolExecution {
		t.Fatalf("expected tool-execution UserError, got %v", err)
	}
	if !strings.Contains(ue.Message, "diff") {
		t.Fatalf("expected message to mention diff, got %q", ue.Message)
	}
}

// TestSummarizeDiff_CountsHunkLines is what the UI uses to render a
// "+12 −3" diff badge during plan review without reading the file.
func TestSummarizeDiff_CountsHunkLines(t *testing.T) {
	diff := `--- a/foo.txt
+++ b/foo.txt
@@ -1,3 +1,3 @@
 keep
-old
+new
 keep
--- a/bar.txt
+++ b/bar.txt
@@ -0,0 +1,2 @@
+a
+b
`
	files, added, removed, err := SummarizeDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 || removed != 1 {
		t.Fatalf("counts = +%d -%d, want +3 -1", added, removed)
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	if !have["foo.txt"] || !have["bar.txt"] {
		t.Fatalf("expected foo.txt and bar.txt in files, got %v", files)
	}
}

// TestSummarizeDiff_RejectsHeaderlessDiff guards against the LLM
// returning a bare hunk; the runtime needs at least one --- a/ +++ b/
// header to know which file to patch.
func TestSummarizeDiff_RejectsHeaderlessDiff(t *testing.T) {
	_, _, _, err := SummarizeDiff(`@@ -1 +1 @@
-old
+new
`)
	if err == nil {
		t.Fatal("expected header-missing error")
	}
}

// TestFilePatchTool_RejectsMissingTargetFile is the new pre-flight:
// the LLM hallucinates a path that doesn't exist; the tool surfaces
// ErrCodePatchFileMissing so the replan loop can read+retry.
func TestFilePatchTool_RejectsMissingTargetFile(t *testing.T) {
	root := t.TempDir()
	diff := `--- a/missing.txt
+++ b/missing.txt
@@ -1 +1 @@
-old
+new
`
	_, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
		"diff":           diff,
	})
	var ue *domain.UserError
	if !errors.As(err, &ue) || ue.Code != domain.ErrCodePatchFileMissing {
		t.Fatalf("expected ErrCodePatchFileMissing, got %v", err)
	}
}

// TestFilePatchTool_RejectsCreateCollision: a `--- /dev/null` create
// block targets a path that already exists. Without this check, git
// apply would fail with a confusing "already exists" message; the
// distinct UserError lets the planner pick a different name.
func TestFilePatchTool_RejectsCreateCollision(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := `--- /dev/null
+++ b/existing.txt
@@ -0,0 +1 @@
+new
`
	_, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
		"diff":           diff,
	})
	var ue *domain.UserError
	if !errors.As(err, &ue) || ue.Code != domain.ErrCodeToolExecution {
		t.Fatalf("expected tool-execution UserError for create collision, got %v", err)
	}
	if !strings.Contains(ue.Message, "already exists") {
		t.Fatalf("expected collision message, got %q", ue.Message)
	}
}

// TestFilePatchTool_RejectsOversizedDiff caps the diff at MaxPatchBytes
// so a runaway LLM cannot OOM the daemon.
func TestFilePatchTool_RejectsOversizedDiff(t *testing.T) {
	root := t.TempDir()
	huge := strings.Repeat("x", MaxPatchBytes+1)
	diff := "--- a/file\n+++ b/file\n@@ -1 +1 @@\n+" + huge + "\n"
	_, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
		"diff":           diff,
	})
	var ue *domain.UserError
	if !errors.As(err, &ue) || ue.Code != domain.ErrCodePatchTooLarge {
		t.Fatalf("expected ErrCodePatchTooLarge, got %v", err)
	}
}

// TestFilePatchTool_AcceptsCreate covers the new-file path: create
// block + non-existent target = success.
func TestFilePatchTool_AcceptsCreate(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	diff := `--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
`
	out, err := NewFilePatchTool().Execute(context.Background(), map[string]interface{}{
		"workspace_root": root,
		"diff":           diff,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok, _ := out["success"].(bool); !ok {
		t.Fatalf("expected success, got %+v", out)
	}
	body, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil {
		t.Fatalf("create did not write file: %v", err)
	}
	if string(body) != "hello\n" {
		t.Fatalf("file body = %q, want %q", body, "hello\n")
	}
}
