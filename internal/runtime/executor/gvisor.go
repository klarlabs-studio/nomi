package executor

import (
	"context"
	"os/exec"
	"strings"
)

// GvisorBackend runs subprocesses through Docker with the runsc (gVisor)
// runtime, giving the container a user-space kernel boundary rather than
// the host kernel. Same hardening as DockerBackend (network=none, pinned
// memory + cpus, --init, --rm) plus the runsc syscall sandbox.
//
// Implemented as a thin composition over DockerBackend so the
// container-wiring code path stays single-sourced.
type GvisorBackend struct {
	inner *DockerBackend
}

// NewGvisor returns a GvisorBackend with conservative defaults matching
// NewDocker. The underlying docker runtime is pinned to "runsc".
func NewGvisor() *GvisorBackend {
	inner := NewDocker()
	inner.Runtime = "runsc"
	return &GvisorBackend{inner: inner}
}

// Name returns "gvisor".
func (GvisorBackend) Name() string { return BackendGvisor }

// Run delegates to the underlying DockerBackend with --runtime=runsc set.
func (g *GvisorBackend) Run(ctx context.Context, req Request) (*Result, error) {
	return g.inner.Run(ctx, req)
}

// Available reports whether Docker is reachable AND the runsc runtime is
// registered with the daemon. Boot probes use this to decide whether to
// register the backend at all.
func (g *GvisorBackend) Available(ctx context.Context) bool {
	if !g.inner.Available(ctx) {
		return false
	}
	binary := g.inner.Binary
	if binary == "" {
		binary = "docker"
	}
	out, err := exec.CommandContext(ctx, binary, "info", "--format", "{{range $k, $v := .Runtimes}}{{$k}} {{end}}").Output() //nolint:gosec // G204: docker binary path from trusted executor config
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "runsc")
}
