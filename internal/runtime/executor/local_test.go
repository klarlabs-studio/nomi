package executor

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalRunSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix echo")
	}
	res, err := NewLocal().Run(context.Background(), Request{
		Argv: []string{"echo", "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
	if !strings.Contains(string(res.Output), "hi") {
		t.Fatalf("output missing payload: %q", res.Output)
	}
}

func TestLocalRunNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	res, err := NewLocal().Run(context.Background(), Request{
		Argv: []string{"sh", "-c", "exit 3"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d", res.ExitCode)
	}
}

func TestLocalRunTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	res, err := NewLocal().Run(context.Background(), Request{
		Argv:    []string{"sleep", "5"},
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("expected TimedOut=true")
	}
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1 on timeout, got %d", res.ExitCode)
	}
}

func TestLocalRunMissingBinary(t *testing.T) {
	res, err := NewLocal().Run(context.Background(), Request{
		Argv: []string{"/this/binary/does/not/exist"},
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if res == nil || res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %+v", res)
	}
}

func TestLocalRunEmptyArgv(t *testing.T) {
	res, err := NewLocal().Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected ErrNotStarted for empty argv")
	}
	if res.ExitCode != -1 {
		t.Fatalf("expected ExitCode -1, got %d", res.ExitCode)
	}
}

func TestRegistryResolveDefault(t *testing.T) {
	reg := NewRegistry()
	local := NewLocal()
	reg.Register(local)
	if got := reg.Resolve(""); got != local {
		t.Fatal("empty name should resolve to default")
	}
	if got := reg.Resolve("unknown"); got != local {
		t.Fatal("unknown name should fall back to default")
	}
	if got := reg.Resolve("local"); got != local {
		t.Fatal("explicit name should resolve")
	}
}

func TestRegistryEmpty(t *testing.T) {
	reg := NewRegistry()
	if got := reg.Resolve(""); got != nil {
		t.Fatalf("empty registry should return nil, got %v", got)
	}
}

func TestRegistryNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewLocal())
	names := reg.Names()
	if len(names) != 1 || names[0] != BackendLocal {
		t.Fatalf("unexpected names: %v", names)
	}
}

func TestSysProcAttrNonNil(t *testing.T) {
	if sysProcAttr() == nil {
		t.Fatal("expected non-nil SysProcAttr")
	}
}
