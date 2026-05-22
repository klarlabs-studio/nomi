package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/runtime/executor"
)

type fakeBackend struct {
	name   string
	result *executor.Result
	err    error
}

func (f fakeBackend) Name() string { return f.name }
func (f fakeBackend) Run(_ context.Context, _ executor.Request) (*executor.Result, error) {
	return f.result, f.err
}

func TestClassifyExecutorOutcomeBuckets(t *testing.T) {
	tests := []struct {
		name string
		res  *executor.Result
		err  error
		want string
	}{
		{"error", nil, errors.New("boom"), "error"},
		{"nil-result-no-err", nil, nil, "error"},
		{"timeout", &executor.Result{TimedOut: true, ExitCode: -1}, nil, "timeout"},
		{"oom", &executor.Result{OOM: true, ExitCode: 137}, nil, "oom"},
		{"exit-nonzero", &executor.Result{ExitCode: 2}, nil, "exit_nonzero"},
		{"success", &executor.Result{ExitCode: 0}, nil, "success"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyExecutorOutcome(tc.res, tc.err); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstrumentedBackendDelegates(t *testing.T) {
	inner := fakeBackend{name: "fake", result: &executor.Result{ExitCode: 0}}
	wrapped := instrumentedBackend{inner: inner}
	if wrapped.Name() != "fake" {
		t.Fatal("Name() should delegate to inner backend")
	}
	res, err := wrapped.Run(context.Background(), executor.Request{Argv: []string{"x"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("unexpected exit code: %d", res.ExitCode)
	}
}
