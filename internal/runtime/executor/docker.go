package executor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// DockerBackend runs subprocesses inside a fresh container, bind-mounting
// the assistant's workspace at /workspace and isolating CPU, memory, PIDs,
// and network from the host. Implemented as a thin wrapper around the
// `docker` CLI to avoid pulling the Docker SDK (and its many transitive
// dependencies) into the daemon binary.
//
// Hardening currently applied per container:
//   - --network=none — no outbound connectivity (PR4 will wire an opt-in
//     network.egress capability and switch to a restricted bridge).
//   - --memory + --memory-swap pinned equal so OOMKilled fires consistently
//     and the kernel doesn't quietly thrash swap.
//   - --cpus + --pids-limit to bound runaway loops.
//   - --init so PID 1 reaps zombies if the image's entrypoint doesn't.
//   - --rm so containers don't accumulate; the trade-off is that
//     post-mortem inspection of OOM details isn't available. We rely on
//     exit code 137 as an OOM heuristic (see Run).
type DockerBackend struct {
	Binary      string // docker CLI path, default "docker"
	Runtime     string // docker --runtime; empty = host default; "runsc" = gVisor
	MemoryLimit string // docker --memory format, default "512m"
	CPULimit    string // docker --cpus format, default "1.0"
	PIDsLimit   int    // docker --pids-limit, default 128
}

// NewDocker returns a DockerBackend with conservative defaults. Override
// the fields after construction when the deployment needs different limits.
func NewDocker() *DockerBackend {
	return &DockerBackend{
		Binary:      "docker",
		MemoryLimit: "512m",
		CPULimit:    "1.0",
		PIDsLimit:   128,
	}
}

// Name returns "docker".
func (DockerBackend) Name() string { return BackendDocker }

// Available reports whether the docker CLI is on PATH and `docker info`
// succeeds. Boot probes use this to decide whether to register the backend
// at all so users without Docker installed don't see it in the UI.
func (d DockerBackend) Available(ctx context.Context) bool {
	binary := d.Binary
	if binary == "" {
		binary = "docker"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, binary, "info", "--format", "{{.ServerVersion}}")
	return cmd.Run() == nil
}

// Run executes the request inside a fresh container. Returns a non-nil
// error only when the backend couldn't start the container at all; a
// non-zero exit (including OOM kills) returns a nil error and a populated
// Result.
func (d DockerBackend) Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Argv) == 0 {
		return &Result{ExitCode: -1}, ErrNotStarted
	}
	if req.Image == "" {
		return &Result{ExitCode: -1}, errors.New("docker backend: image is required")
	}
	if req.WorkspaceRoot == "" {
		return &Result{ExitCode: -1}, errors.New("docker backend: workspace root is required (bind-mount target)")
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	args, err := d.buildArgs(req)
	if err != nil {
		return &Result{ExitCode: -1}, err
	}

	binary := d.Binary
	if binary == "" {
		binary = "docker"
	}
	cmd := exec.CommandContext(runCtx, binary, args...)

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
			// Exit 137 = process killed by SIGKILL. Under our pinned memory
			// + swap config this is almost always OOM; rare false positives
			// can occur if the host kills `docker run` itself.
			if result.ExitCode == 137 {
				result.OOM = true
			}
			return result, nil
		}
		result.ExitCode = -1
		return result, runErr
	}

	result.ExitCode = 0
	return result, nil
}

// buildArgs constructs the `docker run` argv. Exposed for testing so the
// container-flag wiring can be asserted without spawning a real container.
func (d DockerBackend) buildArgs(req Request) ([]string, error) {
	memory := d.MemoryLimit
	if memory == "" {
		memory = "512m"
	}
	cpus := d.CPULimit
	if cpus == "" {
		cpus = "1.0"
	}
	pids := d.PIDsLimit
	if pids <= 0 {
		pids = 128
	}

	containerWorkDir, err := containerWorkDir(req.WorkspaceRoot, req.WorkDir)
	if err != nil {
		return nil, err
	}

	netMode := req.NetworkMode
	if netMode == "" {
		netMode = NetworkNone
	}

	args := []string{
		"run", "--rm",
		"--network=" + string(netMode),
		"--memory=" + memory,
		"--memory-swap=" + memory,
		"--cpus=" + cpus,
		fmt.Sprintf("--pids-limit=%d", pids),
		"--init",
		"-v", req.WorkspaceRoot + ":/workspace",
		"-w", containerWorkDir,
	}
	if d.Runtime != "" {
		args = append(args, "--runtime="+d.Runtime)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	args = append(args, req.Image)
	args = append(args, req.Argv...)
	return args, nil
}

// containerWorkDir translates a host workdir into a path inside the
// container's bind-mounted /workspace. Returns an error if WorkDir escapes
// WorkspaceRoot.
func containerWorkDir(workspaceRoot, hostWorkDir string) (string, error) {
	if hostWorkDir == "" || hostWorkDir == workspaceRoot {
		return "/workspace", nil
	}
	rel, err := filepath.Rel(workspaceRoot, hostWorkDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("docker backend: working_directory escapes workspace_root")
	}
	return filepath.ToSlash(filepath.Join("/workspace", rel)), nil
}
