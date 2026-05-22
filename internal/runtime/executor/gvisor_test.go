package executor

import (
	"context"
	"testing"
)

func TestGvisorName(t *testing.T) {
	if NewGvisor().Name() != BackendGvisor {
		t.Fatal("expected gvisor name")
	}
}

func TestGvisorRuntimeFlag(t *testing.T) {
	g := NewGvisor()
	if g.inner.Runtime != "runsc" {
		t.Fatalf("expected runsc runtime, got %q", g.inner.Runtime)
	}
	args, err := g.inner.buildArgs(Request{
		Argv:          []string{"true"},
		WorkspaceRoot: "/tmp",
		Image:         "alpine:3.20",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !contains(args, "--runtime=runsc") {
		t.Fatalf("expected --runtime=runsc in args, got %v", args)
	}
}

func TestGvisorAvailableSkipsWithoutRunsc(t *testing.T) {
	// Construct a backend whose docker binary doesn't exist. Available()
	// must return false rather than panicking on the missing CLI.
	g := &GvisorBackend{inner: &DockerBackend{Binary: "/does/not/exist"}}
	if g.Available(context.Background()) {
		t.Fatal("expected Available()=false when docker missing")
	}
}
