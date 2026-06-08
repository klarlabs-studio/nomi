package runtime

import (
	"context"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

// TestTransitionRun_ConcurrentLoserGetsErrConcurrentTransition spins
// two goroutines trying to move the same run from RunCreated to two
// different next states. The CAS in transitionRun must let exactly one
// win; the other must surface ErrConcurrentTransition. Without the CAS
// both would silently succeed and the run.* event chain would carry a
// duplicated transition.
func TestTransitionRun_ConcurrentLoserGetsErrConcurrentTransition(t *testing.T) {
	rt, cleanup := setupTestRuntime(t)
	defer cleanup()

	// Seed an assistant + run row directly through the repos so we
	// control the starting status without racing CreateRun's executor
	// goroutine.
	if err := rt.assistantRepo.Create(&domain.AssistantDefinition{
		ID: "a", Name: "A", Role: "x",
	}); err != nil {
		t.Fatalf("seed assistant: %v", err)
	}
	run := &domain.Run{
		ID:          "run-cas-test",
		Goal:        "concurrent test",
		AssistantID: "a",
		Status:      domain.RunCreated,
		PlanVersion: 1,
	}
	if err := rt.runRepo.Create(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var (
		wg         sync.WaitGroup
		successes  int
		concurrent int
		errsMu     sync.Mutex
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		runCopy := *run
		err := rt.transitionRun(context.Background(), &runCopy, domain.RunPlanning)
		errsMu.Lock()
		defer errsMu.Unlock()
		if err == nil {
			successes++
			return
		}
		if err == ErrConcurrentTransition {
			concurrent++
		} else {
			t.Errorf("unexpected error from goroutine A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		runCopy := *run
		err := rt.transitionRun(context.Background(), &runCopy, domain.RunPlanning)
		errsMu.Lock()
		defer errsMu.Unlock()
		if err == nil {
			successes++
			return
		}
		if err == ErrConcurrentTransition {
			concurrent++
		} else {
			t.Errorf("unexpected error from goroutine B: %v", err)
		}
	}()
	wg.Wait()

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful transition, got %d", successes)
	}
	if concurrent != 1 {
		t.Fatalf("expected exactly 1 ErrConcurrentTransition, got %d", concurrent)
	}
}
