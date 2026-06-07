package statekit

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func TestRunStateMachineHappyPath(t *testing.T) {
	sm := NewRunStateMachine()

	if got := sm.Current(); got != domain.RunCreated {
		t.Fatalf("initial state = %s, want %s", got, domain.RunCreated)
	}

	path := []domain.RunStatus{
		domain.RunPlanning,
		domain.RunExecuting,
		domain.RunCompleted,
	}
	for _, to := range path {
		if err := sm.Transition(to, nil); err != nil {
			t.Fatalf("Transition to %s failed: %v", to, err)
		}
	}
}

func TestRunStateMachineRetryFromTerminalStates(t *testing.T) {
	// #15 added terminal→Created edges so RetryRun is a state-machine move.
	terminals := []domain.RunStatus{
		domain.RunFailed,
		domain.RunCancelled,
		domain.RunCompleted,
	}
	for _, start := range terminals {
		t.Run(string(start), func(t *testing.T) {
			sm := NewRunStateMachine()
			sm.SetCurrent(start)
			if err := sm.Transition(domain.RunCreated, nil); err != nil {
				t.Fatalf("retry edge %s→created should be valid: %v", start, err)
			}
			if sm.Current() != domain.RunCreated {
				t.Fatalf("not advanced: %s", sm.Current())
			}
		})
	}
}

func TestRunStateMachineForbidsIllegalDirectJumps(t *testing.T) {
	cases := []struct {
		from domain.RunStatus
		to   domain.RunStatus
	}{
		{domain.RunCreated, domain.RunCompleted},   // can't skip planning/executing
		{domain.RunCreated, domain.RunExecuting},   // must go through planning
		{domain.RunCompleted, domain.RunExecuting}, // terminal → active (only via retry)
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			sm := NewRunStateMachine()
			sm.SetCurrent(tc.from)
			if err := sm.Transition(tc.to, nil); err == nil {
				t.Fatalf("expected %s→%s to be rejected", tc.from, tc.to)
			}
		})
	}
}

func TestRunStateMachineRejectsInvalidStatus(t *testing.T) {
	sm := NewRunStateMachine()
	if err := sm.Transition(domain.RunStatus("nonsense"), nil); err == nil {
		t.Fatal("expected Transition to reject an invalid status string")
	}
}

func TestRunStateMachineAwaitingApprovalRoundTrip(t *testing.T) {
	sm := NewRunStateMachine()
	sm.SetCurrent(domain.RunExecuting)
	if err := sm.Transition(domain.RunAwaitingApproval, nil); err != nil {
		t.Fatalf("executing→awaiting_approval: %v", err)
	}
	if err := sm.Transition(domain.RunExecuting, nil); err != nil {
		t.Fatalf("awaiting_approval→executing: %v", err)
	}
}

func TestStepStateMachineHappyPath(t *testing.T) {
	sm := NewStepStateMachine()

	if got := sm.Current(); got != domain.StepPending {
		t.Fatalf("initial step state = %s, want %s", got, domain.StepPending)
	}

	path := []domain.StepStatus{
		domain.StepReady,
		domain.StepRunning,
		domain.StepDone,
	}
	for _, to := range path {
		if err := sm.Transition(to, nil); err != nil {
			t.Fatalf("step transition to %s failed: %v", to, err)
		}
	}
}

func TestStepStateMachineBlockedRoundTrip(t *testing.T) {
	// running → blocked (awaiting approval) → running (approved) → done
	sm := NewStepStateMachine()
	sm.SetCurrent(domain.StepRunning)
	if err := sm.Transition(domain.StepBlocked, nil); err != nil {
		t.Fatalf("running→blocked: %v", err)
	}
	if err := sm.Transition(domain.StepRunning, nil); err != nil {
		t.Fatalf("blocked→running: %v", err)
	}
	if err := sm.Transition(domain.StepDone, nil); err != nil {
		t.Fatalf("running→done: %v", err)
	}
}

func TestStepStateMachineRetryCycle(t *testing.T) {
	sm := NewStepStateMachine()
	sm.SetCurrent(domain.StepFailed)
	if err := sm.Transition(domain.StepRetrying, nil); err != nil {
		t.Fatalf("failed→retrying: %v", err)
	}
	if err := sm.Transition(domain.StepRunning, nil); err != nil {
		t.Fatalf("retrying→running: %v", err)
	}
}

func TestStepStateMachineForbidsSkippingReady(t *testing.T) {
	sm := NewStepStateMachine()
	// pending → running is NOT declared; must go through ready.
	if err := sm.Transition(domain.StepRunning, nil); err == nil {
		t.Fatal("pending→running should require going through ready")
	}
}

func TestStepStateMachineValidTransitions(t *testing.T) {
	sm := NewStepStateMachine()
	sm.SetCurrent(domain.StepRunning)
	valid := sm.ValidTransitions()
	seen := map[domain.StepStatus]bool{}
	for _, s := range valid {
		seen[s] = true
	}
	// From running: done, failed, blocked.
	for _, expected := range []domain.StepStatus{domain.StepDone, domain.StepFailed, domain.StepBlocked} {
		if !seen[expected] {
			t.Errorf("expected %s in ValidTransitions from running, got %v", expected, valid)
		}
	}
}
