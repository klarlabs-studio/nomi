package executor

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/felixgeelhaar/nomi/internal/runtime/executor/egress"
)

func TestDockerBuildArgs(t *testing.T) {
	d := NewDocker()
	args, err := d.buildArgs(Request{
		Argv:          []string{"echo", "hi"},
		WorkspaceRoot: "/host/work",
		Image:         "alpine:3.20",
		Env:           []string{"PATH=/bin", "HOME=/workspace"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		"run", "--rm",
		"--network=" + string(NetworkNone),
		"--memory=512m",
		"--memory-swap=512m",
		"--cpus=1.0",
		"--pids-limit=128",
		"--init",
		"-v", "/host/work:/workspace",
		"-w", "/workspace",
		"-e", "PATH=/bin",
		"-e", "HOME=/workspace",
		"alpine:3.20",
		"echo", "hi",
	}
	if len(args) != len(want) {
		t.Fatalf("arg count mismatch: got %d, want %d\n got: %v\nwant: %v", len(args), len(want), args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d]: got %q, want %q", i, args[i], want[i])
		}
	}
}

func TestDockerBuildArgsSubdirWorkdir(t *testing.T) {
	d := NewDocker()
	args, err := d.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/host/work",
		WorkDir:       "/host/work/pkg",
		Image:         "alpine:3.20",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(args, "-w") {
		t.Fatal("missing -w flag")
	}
	wIdx := indexOf(args, "-w")
	if args[wIdx+1] != "/workspace/pkg" {
		t.Errorf("workdir: got %q, want %q", args[wIdx+1], "/workspace/pkg")
	}
}

func TestDockerBuildArgsNetworkBridge(t *testing.T) {
	d := NewDocker()
	args, err := d.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/host/work",
		Image:         "alpine:3.20",
		NetworkMode:   NetworkBridge,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(args, "--network=bridge") {
		t.Fatalf("expected --network=bridge, got %v", args)
	}
}

func TestDockerBuildArgsWorkdirEscapeRejected(t *testing.T) {
	d := NewDocker()
	_, err := d.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/host/work",
		WorkDir:       "/host/other",
		Image:         "alpine:3.20",
	})
	if err == nil {
		t.Fatal("expected error for workdir escaping root")
	}
}

func TestDockerRunMissingImage(t *testing.T) {
	_, err := NewDocker().Run(context.Background(), Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/tmp",
	})
	if err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestDockerRunMissingWorkspaceRoot(t *testing.T) {
	_, err := NewDocker().Run(context.Background(), Request{
		Argv:  []string{"true"},
		Image: "alpine:3.20",
	})
	if err == nil {
		t.Fatal("expected error for missing workspace root")
	}
}

func TestDockerRunEmptyArgv(t *testing.T) {
	res, err := NewDocker().Run(context.Background(), Request{
		Image:         "alpine:3.20",
		WorkspaceRoot: "/tmp",
	})
	if err == nil {
		t.Fatal("expected error for empty argv")
	}
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %d", res.ExitCode)
	}
}

func TestDockerNameConst(t *testing.T) {
	if NewDocker().Name() != "docker" {
		t.Fatal("expected backend name 'docker'")
	}
}

// TestDockerLiveEcho runs against the real docker daemon. Skipped if
// docker isn't installed or the daemon isn't reachable; opt in to the
// live path with `go test -tags docker_integration` for the dedicated
// integration sweep, but the test is also useful as a smoke check
// whenever docker happens to be available locally.
func TestDockerLiveEcho(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker run semantics differ on windows hosts")
	}
	d := NewDocker()
	if !d.Available(context.Background()) {
		t.Skip("docker not available")
	}

	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := d.Run(ctx, Request{
		Argv:          []string{"echo", "hi-from-container"},
		WorkspaceRoot: tmp,
		Image:         "alpine:3.20",
		Timeout:       30 * time.Second,
	})
	if err != nil {
		t.Fatalf("docker run failed: %v\noutput: %s", err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\noutput: %s", res.ExitCode, res.Output)
	}
	if !strings.Contains(string(res.Output), "hi-from-container") {
		t.Fatalf("output missing payload: %q", res.Output)
	}
}

func TestDockerBuildArgsHostAllowlist(t *testing.T) {
	prev := resolveHost
	resolveHost = func(host string) ([]string, error) {
		switch host {
		case "api.openai.com":
			return []string{"203.0.113.10"}, nil
		case "graph.facebook.com":
			return []string{"203.0.113.20", "203.0.113.21"}, nil
		}
		return nil, fmt.Errorf("unexpected host %q", host)
	}
	defer func() { resolveHost = prev }()

	d := NewDocker()
	args, err := d.buildArgs(Request{
		Argv:          []string{"curl", "https://api.openai.com"},
		WorkspaceRoot: "/host/work",
		Image:         "alpine:3.20",
		NetworkMode:   NetworkBridge,
		HostAllowlist: []string{"api.openai.com", "graph.facebook.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !contains(args, "--network=bridge") {
		t.Fatalf("expected --network=bridge, got %v", args)
	}
	wantHosts := []string{
		"--add-host=api.openai.com:203.0.113.10",
		"--add-host=graph.facebook.com:203.0.113.20",
		"--add-host=graph.facebook.com:203.0.113.21",
	}
	for _, h := range wantHosts {
		if !contains(args, h) {
			t.Errorf("missing %s in %v", h, args)
		}
	}
	if !contains(args, "--dns="+unroutableDNS) {
		t.Fatalf("missing --dns=%s in %v", unroutableDNS, args)
	}
}

func TestDockerBuildArgsHostAllowlistForcesBridgeFromNone(t *testing.T) {
	prev := resolveHost
	resolveHost = func(string) ([]string, error) { return []string{"203.0.113.10"}, nil }
	defer func() { resolveHost = prev }()

	d := NewDocker()
	args, err := d.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/host/work",
		Image:         "alpine:3.20",
		NetworkMode:   NetworkNone,
		HostAllowlist: []string{"api.openai.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(args, "--network=bridge") {
		t.Fatalf("allowlist on NetworkNone should force bridge, got %v", args)
	}
}

func TestDockerBuildArgsHostAllowlistResolveFailureSurfaces(t *testing.T) {
	prev := resolveHost
	resolveHost = func(string) ([]string, error) { return nil, fmt.Errorf("nope") }
	defer func() { resolveHost = prev }()

	d := NewDocker()
	_, err := d.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/host/work",
		Image:         "alpine:3.20",
		NetworkMode:   NetworkBridge,
		HostAllowlist: []string{"unresolvable.invalid"},
	})
	if err == nil {
		t.Fatal("expected resolve failure to bubble up")
	}
}

func TestDockerBuildArgsNoAllowlistOmitsAddHost(t *testing.T) {
	d := NewDocker()
	args, err := d.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/host/work",
		Image:         "alpine:3.20",
		NetworkMode:   NetworkBridge,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--add-host=") {
			t.Fatalf("did not expect --add-host without an allowlist, got %v", args)
		}
		if strings.HasPrefix(a, "--dns=") {
			t.Fatalf("did not expect --dns without an allowlist, got %v", args)
		}
	}
}

// fakeEgressFilter is a deterministic stand-in for egress.Filter that
// records every AddIP and Close call so the docker-backend integration
// can be unit-tested without a real kernel.
type fakeEgressFilter struct {
	path         string
	dockerParent string
	addIPs       []string
	closed       bool
}

func (f *fakeEgressFilter) CgroupPath() string { return f.path }
func (f *fakeEgressFilter) DockerCgroupParent() string {
	if f.dockerParent != "" {
		return f.dockerParent
	}
	return f.path
}
func (f *fakeEgressFilter) AddIP(ip net.IP) error {
	f.addIPs = append(f.addIPs, ip.String())
	return nil
}
func (f *fakeEgressFilter) Close() error {
	f.closed = true
	return nil
}

func TestDockerRunUsesSystemdSliceForSystemdDriver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker semantics differ on windows")
	}
	prevResolve := resolveHost
	resolveHost = func(_ string) ([]string, error) { return []string{"203.0.113.10"}, nil }
	defer func() { resolveHost = prevResolve }()

	prevDriver := detectCgroupDriver
	detectCgroupDriver = func(_ context.Context, _ string) egress.Driver {
		return egress.DriverSystemd
	}
	defer func() { detectCgroupDriver = prevDriver }()

	var sawConfig egress.Config
	fake := &fakeEgressFilter{
		path:         "/sys/fs/cgroup/nomi-sandbox-abc.slice",
		dockerParent: "nomi-sandbox-abc.slice",
	}
	prevFilter := newEgressFilter
	newEgressFilter = func(cfg egress.Config) (egress.Filter, error) {
		sawConfig = cfg
		return fake, nil
	}
	defer func() { newEgressFilter = prevFilter }()

	t.Setenv("NOMI_EGRESS_EBPF", "1")

	d := NewDocker()
	d.Binary = "/bin/echo"

	res, err := d.Run(context.Background(), Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/tmp",
		Image:         "alpine:3.20",
		HostAllowlist: []string{"api.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, res.Output)
	}
	if sawConfig.DockerCgroupDriver != egress.DriverSystemd {
		t.Errorf("egress.New received driver %q, want %q",
			sawConfig.DockerCgroupDriver, egress.DriverSystemd)
	}
	if !strings.Contains(string(res.Output), "--cgroup-parent=nomi-sandbox-abc.slice") {
		t.Errorf("expected systemd slice name in --cgroup-parent, got %q", res.Output)
	}
	// The absolute /sys/fs/cgroup/... path must NOT appear in the
	// docker args for systemd driver — docker rejects it.
	if strings.Contains(string(res.Output), "--cgroup-parent=/sys/fs/cgroup") {
		t.Errorf("absolute path leaked into systemd-driver argv: %q", res.Output)
	}
}

func TestDockerResolveCgroupDriverCaches(t *testing.T) {
	prev := detectCgroupDriver
	calls := 0
	detectCgroupDriver = func(_ context.Context, _ string) egress.Driver {
		calls++
		return egress.DriverSystemd
	}
	defer func() { detectCgroupDriver = prev }()

	d := NewDocker()
	for i := 0; i < 5; i++ {
		if got := d.resolveCgroupDriver(context.Background()); got != egress.DriverSystemd {
			t.Fatalf("call %d: got %q, want systemd", i, got)
		}
	}
	if calls != 1 {
		t.Errorf("detectCgroupDriver called %d times, want exactly 1 (sync.Once)", calls)
	}
}

func TestDockerRunWithEBPFFilterPopulatesMap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker semantics differ on windows")
	}
	prevResolve := resolveHost
	resolveHost = func(host string) ([]string, error) {
		if host == "api.example.com" {
			return []string{"203.0.113.10"}, nil
		}
		return nil, fmt.Errorf("unexpected host %q", host)
	}
	defer func() { resolveHost = prevResolve }()

	fake := &fakeEgressFilter{path: "/sys/fs/cgroup/nomi-sandbox-fake"}
	prevFilter := newEgressFilter
	newEgressFilter = func(egress.Config) (egress.Filter, error) {
		return fake, nil
	}
	defer func() { newEgressFilter = prevFilter }()

	t.Setenv("NOMI_EGRESS_EBPF", "1")

	d := NewDocker()
	d.Binary = "/bin/echo" // make docker invocation a noop that just echoes args

	res, err := d.Run(context.Background(), Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/tmp",
		Image:         "alpine:3.20",
		NetworkMode:   NetworkBridge,
		HostAllowlist: []string{"api.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, res.Output)
	}
	if !fake.closed {
		t.Error("filter.Close() not called via defer")
	}
	if len(fake.addIPs) != 1 || fake.addIPs[0] != "203.0.113.10" {
		t.Errorf("expected AddIP(203.0.113.10), got %v", fake.addIPs)
	}
	if !strings.Contains(string(res.Output), "--cgroup-parent=/sys/fs/cgroup/nomi-sandbox-fake") {
		t.Errorf("expected --cgroup-parent in argv, got %q", res.Output)
	}
}

func TestDockerRunWithEBPFUnavailableFallsBackToDNSOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker semantics differ on windows")
	}
	prevResolve := resolveHost
	resolveHost = func(_ string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	}
	defer func() { resolveHost = prevResolve }()

	prevFilter := newEgressFilter
	newEgressFilter = func(egress.Config) (egress.Filter, error) {
		return nil, egress.ErrUnsupported
	}
	defer func() { newEgressFilter = prevFilter }()

	t.Setenv("NOMI_EGRESS_EBPF", "1")

	d := NewDocker()
	d.Binary = "/bin/echo"

	res, err := d.Run(context.Background(), Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/tmp",
		Image:         "alpine:3.20",
		HostAllowlist: []string{"api.example.com"},
	})
	if err != nil {
		t.Fatalf("expected DNS-only fallback to succeed, got %v\noutput: %s", err, res.Output)
	}
	if strings.Contains(string(res.Output), "--cgroup-parent") {
		t.Errorf("unsupported filter should not produce --cgroup-parent, got %q", res.Output)
	}
	if !strings.Contains(string(res.Output), "--add-host=api.example.com:203.0.113.10") {
		t.Errorf("DNS-only fallback should still pin via --add-host, got %q", res.Output)
	}
}

func TestDockerRunEBPFDisabledByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker semantics differ on windows")
	}
	prevResolve := resolveHost
	resolveHost = func(_ string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	}
	defer func() { resolveHost = prevResolve }()

	called := false
	prevFilter := newEgressFilter
	newEgressFilter = func(egress.Config) (egress.Filter, error) {
		called = true
		return nil, nil
	}
	defer func() { newEgressFilter = prevFilter }()

	t.Setenv("NOMI_EGRESS_EBPF", "")

	d := NewDocker()
	d.Binary = "/bin/echo"

	_, err := d.Run(context.Background(), Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/tmp",
		Image:         "alpine:3.20",
		HostAllowlist: []string{"api.example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("newEgressFilter must not be called when NOMI_EGRESS_EBPF != 1")
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
