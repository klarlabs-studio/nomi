package evals

import (
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/pkg/statekit"
)

// TestChaos_RunStateMachine verifies run state machine resilience.
func TestChaos_RunStateMachine(t *testing.T) {
	sm := statekit.NewRunStateMachine()
	if sm.Current() != domain.RunCreated {
		t.Fatalf("initial state = %s, want %s", sm.Current(), domain.RunCreated)
	}

	// Valid transitions
	valid := []struct {
		from domain.RunStatus
		to   domain.RunStatus
	}{
		{domain.RunCreated, domain.RunPlanning},
		{domain.RunPlanning, domain.RunExecuting},
		{domain.RunExecuting, domain.RunCompleted},
	}
	for _, v := range valid {
		sm.SetCurrent(v.from)
		if err := sm.Transition(v.to, nil); err != nil {
			t.Fatalf("%s→%s failed: %v", v.from, v.to, err)
		}
	}

	// Terminal states can retry to Created
	terminals := []domain.RunStatus{domain.RunFailed, domain.RunCancelled, domain.RunCompleted}
	for _, start := range terminals {
		sm.SetCurrent(start)
		if err := sm.Transition(domain.RunCreated, nil); err != nil {
			t.Fatalf("retry %s→Created failed: %v", start, err)
		}
	}
}

// TestChaos_StepStateMachine verifies step state machine resilience.
func TestChaos_StepStateMachine(t *testing.T) {
	sm := statekit.NewStepStateMachine()
	if sm.Current() != domain.StepPending {
		t.Fatalf("initial state = %s, want %s", sm.Current(), domain.StepPending)
	}

	// Valid transitions
	valid := []struct {
		from domain.StepStatus
		to   domain.StepStatus
	}{
		{domain.StepPending, domain.StepReady},
		{domain.StepReady, domain.StepRunning},
		{domain.StepRunning, domain.StepDone},
	}
	for _, v := range valid {
		sm.SetCurrent(v.from)
		if err := sm.Transition(v.to, nil); err != nil {
			t.Fatalf("%s→%s failed: %v", v.from, v.to, err)
		}
	}

	// Failed can retry
	sm.SetCurrent(domain.StepFailed)
	if err := sm.Transition(domain.StepRetrying, nil); err != nil {
		t.Fatalf("failed→retrying failed: %v", err)
	}
	if err := sm.Transition(domain.StepRunning, nil); err != nil {
		t.Fatalf("retrying→running failed: %v", err)
	}
}

// TestChaos_IllegalRunTransitions verifies illegal transitions are rejected.
func TestChaos_IllegalRunTransitions(t *testing.T) {
	illegal := []struct {
		from domain.RunStatus
		to   domain.RunStatus
	}{
		{domain.RunCreated, domain.RunCompleted},   // skip planning
		{domain.RunCompleted, domain.RunExecuting}, // terminal→active (no retry)
	}
	for _, tc := range illegal {
		sm := statekit.NewRunStateMachine()
		sm.SetCurrent(tc.from)
		if err := sm.Transition(tc.to, nil); err == nil {
			t.Fatalf("%s→%s should have failed", tc.from, tc.to)
		}
	}
}

// TestChaos_IllegalStepTransitions verifies illegal step transitions are rejected.
func TestChaos_IllegalStepTransitions(t *testing.T) {
	illegal := []struct {
		from domain.StepStatus
		to   domain.StepStatus
	}{
		{domain.StepPending, domain.StepDone},   // skip ready/running
		{domain.StepRunning, domain.StepReady},  // reverse without retry
		{domain.StepDone, domain.StepRunning},   // terminal→active
		{domain.StepFailed, domain.StepRunning}, // terminal→active (no retry)
	}
	for _, tc := range illegal {
		sm := statekit.NewStepStateMachine()
		sm.SetCurrent(tc.from)
		if err := sm.Transition(tc.to, nil); err == nil {
			t.Fatalf("%s→%s should have failed", tc.from, tc.to)
		}
		if sm.Current() != tc.from {
			t.Fatalf("state changed after failed transition: got %s, want %s", sm.Current(), tc.from)
		}
	}
}

// TestChaos_ErrorClassification verifies all error codes are classified.
func TestChaos_ErrorClassification(t *testing.T) {
	allCodes := []string{
		domain.ErrCodeCeilingViolation,
		domain.ErrCodePolicyDeny,
		domain.ErrCodeApprovalDenied,
		domain.ErrCodeApprovalRemembered,
		domain.ErrCodeRateLimited,
		domain.ErrCodeRetryExhausted,
		domain.ErrCodeToolNotFound,
		domain.ErrCodeToolExecution,
		domain.ErrCodePlannerFailed,
		domain.ErrCodeNoLLMProvider,
		domain.ErrCodeLLMInvalidKey,
		domain.ErrCodeLLMRateLimited,
		domain.ErrCodeLLMContextTooLong,
		domain.ErrCodeMissingWorkspace,
		domain.ErrCodePathOutsideRoot,
		domain.ErrCodeBinaryNotAllowed,
		domain.ErrCodeCommandTimeout,
		domain.ErrCodeCommandUnsafe,
	}

	for _, code := range allCodes {
		t.Run(code, func(t *testing.T) {
			err := &domain.UserError{Code: code, Message: "chaos test"}
			got := ClassifyError(err)
			switch got {
			case FailureUnknown, FailurePlanner, FailureToolNotFound,
				FailureToolExecution, FailurePermissionDenied,
				FailureApprovalDenied, FailureRateLimited,
				FailureContextTooLong, FailureNoProviderConfigured,
				FailureValidation:
				// valid
			default:
				t.Fatalf("ClassifyError returned unknown class %q for code %s", got, code)
			}
		})
	}
}
