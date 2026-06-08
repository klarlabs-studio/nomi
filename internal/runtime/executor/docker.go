package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"go.klarlabs.de/nomi/internal/runtime/executor/egress"
)

// egressEBPFEnv is the env var that gates the experimental cgroup_skb
// egress filter. Off by default so v1 behaviour (DNS-only allowlist)
// is unchanged. Set NOMI_EGRESS_EBPF=1 on the daemon process to enable.
const egressEBPFEnv = "NOMI_EGRESS_EBPF"

// newEgressFilter is indirected through a package var so tests on
// non-Linux hosts can swap in a deterministic stub without dragging
// the linux build tag through the test file.
var newEgressFilter = egress.New

// resolveHost is the host-side DNS lookup used to pin allowlisted hosts
// before container start. Indirected through a package var so tests can
// swap in a deterministic resolver without hitting the real network.
var resolveHost = func(host string) ([]string, error) {
	return net.LookupHost(host)
}

// detectCgroupDriver is the function used by the docker backend to
// resolve which cgroup driver the daemon is configured with. Behind a
// package var so tests can stub the binary lookup deterministically.
// Default impl shells out to `docker info --format '{{.CgroupDriver}}'`.
var detectCgroupDriver = func(ctx context.Context, binary string) egress.Driver {
	cmd := exec.CommandContext(ctx, binary, "info", "--format", "{{.CgroupDriver}}")
	out, err := cmd.Output()
	if err != nil {
		// On detection failure (no docker installed, daemon
		// unreachable) fall back to cgroupfs — the safer default,
		// since failing the eBPF path entirely just drops us back to
		// DNS-only.
		return egress.DriverCgroupfs
	}
	switch strings.TrimSpace(string(out)) {
	case "systemd":
		return egress.DriverSystemd
	default:
		return egress.DriverCgroupfs
	}
}

// unroutableDNS is the address container DNS is pointed at when an
// allowlist is in effect. Lookups for anything not pinned via --add-host
// time out against this address. Using a documentation-block-reserved
// IP that isn't routable from any container interface.
const unroutableDNS = "127.255.255.255"

// DockerBackend runs subprocesses inside a fresh container, bind-mounting
// the assistant's workspace at /workspace and isolating CPU, memory, PIDs,
// and network from the host. Implemented as a thin wrapper around the
// `docker` CLI to avoid pulling the Docker SDK (and its many transitive
// dependencies) into the daemon binary.
//
// Hardening currently applied per container:
//   - --network=none by default. An Allow rule on network.egress flips to
//     --network=bridge. If that rule carries a `host_allowlist` constraint,
//     each host is pre-resolved on the host's DNS, pinned via --add-host,
//     and in-container DNS is steered at an unroutable address so lookups
//     for anything outside the list fail.
//   - Optional eBPF cgroup_skb/egress filter (Linux only, gated by
//     NOMI_EGRESS_EBPF=1) — attaches a BPF program to a per-run cgroup
//     and drops any outbound IPv4 packet whose destination isn't in the
//     pre-resolved allowlist. Closes the hardcoded-IP gap left by the
//     DNS-only path. Requires CAP_BPF + CAP_NET_ADMIN and cgroup v2;
//     unsupported environments soft-fail back to DNS-only.
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

	// driverOnce + cachedDriver memoise the cgroup-driver probe so it
	// runs at most once per daemon lifetime. detectCgroupDriver is a
	// non-trivial subprocess; calling it per Run would visibly slow
	// short tool invocations.
	driverOnce   sync.Once
	cachedDriver egress.Driver
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
func (*DockerBackend) Name() string { return BackendDocker }

// resolveCgroupDriver returns the docker daemon's cgroup driver, probed
// exactly once via detectCgroupDriver and cached for the lifetime of
// the backend. Used to pick the right cgroup-naming scheme for the
// eBPF egress filter (.slice for systemd, flat dir for cgroupfs).
func (d *DockerBackend) resolveCgroupDriver(ctx context.Context) egress.Driver {
	d.driverOnce.Do(func() {
		binary := d.Binary
		if binary == "" {
			binary = "docker"
		}
		d.cachedDriver = detectCgroupDriver(ctx, binary)
	})
	return d.cachedDriver
}

// Available reports whether the docker CLI is on PATH and `docker info`
// succeeds. Boot probes use this to decide whether to register the backend
// at all so users without Docker installed don't see it in the UI.
func (d *DockerBackend) Available(ctx context.Context) bool {
	binary := d.Binary
	if binary == "" {
		binary = "docker"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, binary, "info", "--format", "{{.ServerVersion}}") //nolint:gosec // G204: docker binary path from trusted executor config
	return cmd.Run() == nil
}

// Run executes the request inside a fresh container. Returns a non-nil
// error only when the backend couldn't start the container at all; a
// non-zero exit (including OOM kills) returns a nil error and a populated
// Result.
func (d *DockerBackend) Run(ctx context.Context, req Request) (*Result, error) {
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

	// Resolve the allowlist once so --add-host pins and the optional
	// eBPF map agree on the same IP set. If a host fails to resolve
	// this surfaces here rather than from deep inside buildArgs.
	var resolved map[string][]net.IP
	if len(req.HostAllowlist) > 0 {
		r, err := resolveAllowlist(req.HostAllowlist)
		if err != nil {
			return &Result{ExitCode: -1}, err
		}
		resolved = r
	}

	cgroupParent := ""
	if len(req.HostAllowlist) > 0 && os.Getenv(egressEBPFEnv) == "1" {
		filter, err := newEgressFilter(egress.Config{
			DockerCgroupDriver: d.resolveCgroupDriver(runCtx),
		})
		if err != nil {
			// Soft failure: surface a structured warning and continue
			// with the DNS-only path. We never silently drop the
			// allowlist intent — the caller already gets --add-host /
			// --dns enforcement.
			slog.Warn("docker backend: eBPF egress filter unavailable, falling back to DNS-only allowlist",
				"error", err,
			)
		} else {
			for host, ips := range resolved {
				for _, ip := range ips {
					if addErr := filter.AddIP(ip); addErr != nil {
						slog.Warn("docker backend: eBPF allowlist insert failed",
							"host", host, "ip", ip.String(), "error", addErr,
						)
					}
				}
			}
			cgroupParent = filter.DockerCgroupParent()
			defer func() {
				if closeErr := filter.Close(); closeErr != nil {
					slog.Warn("docker backend: eBPF egress filter cleanup error", "error", closeErr)
				}
			}()
		}
	}

	args, err := d.buildArgsResolved(req, resolved, cgroupParent)
	if err != nil {
		return &Result{ExitCode: -1}, err
	}

	binary := d.Binary
	if binary == "" {
		binary = "docker"
	}
	cmd := exec.CommandContext(runCtx, binary, args...) //nolint:gosec // G204: docker binary + args built by the executor, not user shell input

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

// buildArgs is the thin entry point used by tests and any callsite
// that doesn't need to coordinate with the eBPF filter. It resolves
// the allowlist itself and never sets --cgroup-parent.
func (d *DockerBackend) buildArgs(req Request) ([]string, error) {
	return d.buildArgsResolved(req, nil, "")
}

// buildArgsResolved is the full version Run() uses so the eBPF filter
// path can share a single allowlist resolution and inject
// --cgroup-parent. Passing `resolved == nil` falls back to resolving
// here so direct callers (tests, audit-time dry runs) still work.
func (d *DockerBackend) buildArgsResolved(req Request, resolved map[string][]net.IP, cgroupParent string) ([]string, error) {
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
	// An allowlist on a none-network request is nonsensical; force bridge
	// so the pinned hosts are actually reachable. Empty allowlist =
	// caller's mode is honoured as-is.
	if len(req.HostAllowlist) > 0 && netMode == NetworkNone {
		netMode = NetworkBridge
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
	if len(req.HostAllowlist) > 0 {
		if resolved == nil {
			r, err := resolveAllowlist(req.HostAllowlist)
			if err != nil {
				return nil, err
			}
			resolved = r
		}
		args = append(args, buildHostAllowlistArgs(resolved)...)
	}
	if cgroupParent != "" {
		args = append(args, "--cgroup-parent="+cgroupParent)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	args = append(args, req.Image)
	args = append(args, req.Argv...)
	return args, nil
}

// resolveAllowlist resolves each hostname once on the host's DNS so
// every downstream consumer (--add-host pins, optional eBPF allowlist
// map) sees the same IP set. An unresolvable host is a hard error:
// silently dropping it would let the assistant reach unintended
// endpoints if upstream DNS later recovers and returns a different IP.
func resolveAllowlist(hosts []string) (map[string][]net.IP, error) {
	out := make(map[string][]net.IP, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		raw, err := resolveHost(h)
		if err != nil {
			return nil, fmt.Errorf("docker backend: host_allowlist resolve %q: %w", h, err)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("docker backend: host_allowlist %q has no A/AAAA records", h)
		}
		ips := make([]net.IP, 0, len(raw))
		for _, s := range raw {
			if parsed := net.ParseIP(s); parsed != nil {
				ips = append(ips, parsed)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("docker backend: host_allowlist %q produced no parseable IPs", h)
		}
		out[h] = ips
	}
	return out, nil
}

// buildHostAllowlistArgs renders --add-host pins + --dns flags from a
// pre-resolved allowlist. Sorted by host name to keep arg order stable
// for tests and any downstream eq-checking.
func buildHostAllowlistArgs(resolved map[string][]net.IP) []string {
	hosts := make([]string, 0, len(resolved))
	for h := range resolved {
		hosts = append(hosts, h)
	}
	// Deterministic ordering — Go map iteration is unspecified and
	// docker_test.go asserts on the result.
	sortStrings(hosts)
	var out []string
	for _, h := range hosts {
		for _, ip := range resolved[h] {
			out = append(out, "--add-host="+h+":"+ip.String())
		}
	}
	out = append(out, "--dns="+unroutableDNS)
	return out
}

// sortStrings is a tiny wrapper so we don't reach for sort.Strings just
// to sort a one-shot host slice — keeps the import list lean.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
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
