package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/runtime/executor"
)

// CommandExecTool executes shell commands with strict argv parsing and a
// clean environment. The actual process spawn is delegated to an
// executor.Backend supplied by the runtime via input["__sandbox"]; falls
// back to a fresh LocalBackend for direct test invocations.
type CommandExecTool struct{}

// NewCommandExecTool creates a new CommandExecTool
func NewCommandExecTool() *CommandExecTool {
	return &CommandExecTool{}
}

// Name returns the tool name
func (t *CommandExecTool) Name() string {
	return "command.exec"
}

// Capability returns the required capability
func (t *CommandExecTool) Capability() string {
	return "command.exec"
}

// Execute runs a single binary with its arguments.
//
// Input fields:
//   - command (string, required): the command line. Parsed like a POSIX shell
//     but executed directly (no /bin/sh). Metacharacters are refused.
//   - workspace_root (string, optional): if set, cwd must resolve inside it,
//     and an unset working_directory defaults to the root.
//   - working_directory (string, optional): cwd for the subprocess; validated
//     against workspace_root when both are set.
//   - env (map[string]string, optional): overrides merged onto the env
//     allowlist. The daemon's other env vars (secrets, tokens) are never
//     forwarded.
//   - allowed_binaries ([]string, optional): if non-empty, only argv[0]s with
//     a basename in this list are allowed to run.
//   - timeout (number, optional): seconds; default 30.
func (t *CommandExecTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	rawCommand, ok := input["command"].(string)
	if !ok || rawCommand == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Missing command",
			Message: "Nomi needs a command to run. The planner may have forgotten to include it.",
		}
	}

	tokens, err := ParseCommand(rawCommand)
	if err != nil {
		return nil, err
	}

	if rawList, ok := input["allowed_binaries"].([]interface{}); ok && len(rawList) > 0 {
		binary := filepath.Base(tokens[0])
		permitted := false
		for _, entry := range rawList {
			if s, ok := entry.(string); ok && s == binary {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, &domain.UserError{
				Code:    domain.ErrCodeBinaryNotAllowed,
				Title:   "Command not allowed",
				Message: fmt.Sprintf("The command %q isn't on the allowed list for this assistant. Open the assistant builder and add it to the allowed binaries, or change the permission rule to Confirm.", binary),
				Action:  "Open Assistant Builder",
			}
		}
	}

	workRoot, err := WorkspaceRootFromInput(input)
	if err != nil {
		return nil, err
	}

	workDir := ""
	if raw, ok := input["working_directory"].(string); ok && raw != "" {
		if workRoot != "" {
			resolved, err := ResolveWithinRoot(workRoot, raw)
			if err != nil {
				return nil, fmt.Errorf("working_directory: %w", err)
			}
			workDir = resolved
		} else {
			workDir = filepath.Clean(raw)
		}
	} else if workRoot != "" {
		workDir = workRoot
	}

	timeout := 30
	if v, ok := input["timeout"].(float64); ok {
		timeout = int(v)
	} else if v, ok := input["timeout"].(int); ok {
		timeout = v
	}

	overrides := map[string]string{}
	if rawEnv, ok := input["env"].(map[string]interface{}); ok {
		for k, v := range rawEnv {
			overrides[k] = fmt.Sprintf("%v", v)
		}
	}

	backend := backendFromInput(input)

	image, _ := input["__sandbox_image"].(string)
	netMode, _ := input["__network_mode"].(string)
	hostAllowlist := hostAllowlistFromInput(input)

	req := executor.Request{
		Argv:          tokens,
		WorkDir:       workDir,
		WorkspaceRoot: workRoot,
		Image:         image,
		Env:           BuildSandboxEnv(overrides),
		Timeout:       time.Duration(timeout) * time.Second,
		NetworkMode:   executor.NetworkMode(netMode),
		HostAllowlist: hostAllowlist,
	}

	execResult, runErr := backend.Run(ctx, req)
	if execResult == nil {
		execResult = &executor.Result{ExitCode: -1}
	}

	result := map[string]interface{}{
		"command":   rawCommand,
		"argv":      tokens,
		"output":    string(execResult.Output),
		"exit_code": execResult.ExitCode,
		"work_dir":  workDir,
		"timed_out": execResult.TimedOut,
		"backend":   backend.Name(),
	}

	if execResult.TimedOut {
		return result, &domain.UserError{
			Code:    domain.ErrCodeCommandTimeout,
			Title:   "Command took too long",
			Message: fmt.Sprintf("The command didn't finish within %d seconds. Try a simpler task or increase the timeout.", timeout),
		}
	}

	if runErr != nil {
		result["error"] = runErr.Error()
		return result, nil
	}

	if execResult.ExitCode != 0 {
		result["error"] = fmt.Sprintf("exit status %d", execResult.ExitCode)
	}

	return result, nil
}

// backendFromInput resolves the executor backend the runtime injected into
// the tool input. Falls back to a fresh local backend so direct test calls
// (without a runtime in front) still work.
func backendFromInput(input map[string]interface{}) executor.Backend {
	if b, ok := input["__sandbox"].(executor.Backend); ok && b != nil {
		return b
	}
	return executor.NewLocal()
}

// hostAllowlistFromInput pulls the reserved `__host_allowlist` key the
// runtime injects when an Allow rule on network.egress carries a
// host_allowlist constraint. Accepts both []string (runtime fast path)
// and []interface{} (JSON-decoded shape for any test caller).
func hostAllowlistFromInput(input map[string]interface{}) []string {
	raw, ok := input["__host_allowlist"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
