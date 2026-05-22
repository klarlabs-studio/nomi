package executor

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
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
