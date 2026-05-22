package executor

import (
	"context"
	"os/exec"
)

// LocalBackend runs subprocesses directly on the host inside a fresh session/
// process group. Matches Nomi's pre-sandboxing behavior — kept as the default
// for upgrades and for users who don't want a container runtime in the loop.
type LocalBackend struct{}

// NewLocal returns the local backend.
func NewLocal() *LocalBackend { return &LocalBackend{} }

// Name returns "local".
func (LocalBackend) Name() string { return BackendLocal }

// Run executes the request in a child process and collects combined output.
// A nil error with non-zero ExitCode means the process ran but failed; a
// non-nil error means the backend couldn't start the process at all.
func (LocalBackend) Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Argv) == 0 {
		return &Result{ExitCode: -1}, ErrNotStarted
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, req.Argv[0], req.Argv[1:]...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	if req.Env != nil {
		cmd.Env = req.Env
	}
	cmd.SysProcAttr = sysProcAttr()

	output, runErr := cmd.CombinedOutput()
	result := &Result{Output: output}

	if runCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.ExitCode = -1
		return result, nil
	}

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		result.ExitCode = -1
		return result, runErr
	}

	result.ExitCode = 0
	return result, nil
}
