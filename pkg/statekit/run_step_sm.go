package statekit

import (
	"fmt"

	"go.klarlabs.de/nomi/internal/domain"
)

// RunStateMachine manages the lifecycle of a Run
type RunStateMachine struct {
	machine *Machine
}

// NewRunStateMachine creates a new Run state machine
func NewRunStateMachine() *RunStateMachine {
	m := NewMachine(State(domain.RunCreated))

	// Define valid states
	m.AddState(State(domain.RunPlanning))
	m.AddState(State(domain.RunPlanReview))
	m.AddState(State(domain.RunAwaitingApproval))
	m.AddState(State(domain.RunExecuting))
	m.AddState(State(domain.RunPaused))
	m.AddState(State(domain.RunCompleted))
	m.AddState(State(domain.RunFailed))
	m.AddState(State(domain.RunCancelled))

	// Define transitions
	m.AddTransition(State(domain.RunCreated), State(domain.RunPlanning), nil)
	m.AddTransition(State(domain.RunPlanning), State(domain.RunPlanReview), nil)
	m.AddTransition(State(domain.RunPlanning), State(domain.RunExecuting), nil)
	m.AddTransition(State(domain.RunPlanning), State(domain.RunFailed), nil)
	m.AddTransition(State(domain.RunPlanReview), State(domain.RunExecuting), nil)
	m.AddTransition(State(domain.RunPlanReview), State(domain.RunPlanning), nil)
	m.AddTransition(State(domain.RunPlanReview), State(domain.RunFailed), nil)
	m.AddTransition(State(domain.RunExecuting), State(domain.RunAwaitingApproval), nil)
	m.AddTransition(State(domain.RunExecuting), State(domain.RunCompleted), nil)
	m.AddTransition(State(domain.RunExecuting), State(domain.RunFailed), nil)
	m.AddTransition(State(domain.RunExecuting), State(domain.RunPaused), nil)
	m.AddTransition(State(domain.RunAwaitingApproval), State(domain.RunExecuting), nil)
	m.AddTransition(State(domain.RunAwaitingApproval), State(domain.RunFailed), nil)
	m.AddTransition(State(domain.RunPaused), State(domain.RunExecuting), nil)
	m.AddTransition(State(domain.RunPaused), State(domain.RunCancelled), nil)
	m.AddTransition(State(domain.RunCreated), State(domain.RunCancelled), nil)
	m.AddTransition(State(domain.RunPlanning), State(domain.RunCancelled), nil)
	m.AddTransition(State(domain.RunPlanReview), State(domain.RunCancelled), nil)

	// Retry edges: a terminal run can be re-entered into Created so a new
	// plan version can be generated and executed. Without these edges,
	// RetryRun would have to bypass the state machine entirely.
	m.AddTransition(State(domain.RunFailed), State(domain.RunCreated), nil)
	m.AddTransition(State(domain.RunCancelled), State(domain.RunCreated), nil)
	m.AddTransition(State(domain.RunCompleted), State(domain.RunCreated), nil)

	return &RunStateMachine{machine: m}
}

// Current returns the current state
func (sm *RunStateMachine) Current() domain.RunStatus {
	return domain.RunStatus(sm.machine.Current())
}

// SetCurrent sets the current state from a RunStatus
func (sm *RunStateMachine) SetCurrent(status domain.RunStatus) {
	sm.machine.SetCurrent(State(status))
}

// Transition attempts to transition to a new state
func (sm *RunStateMachine) Transition(to domain.RunStatus, context interface{}) error {
	if !to.IsValid() {
		return fmt.Errorf("invalid run status: %s", to)
	}
	return sm.machine.Transition(State(to), context)
}

// CanTransition checks if a transition is valid
func (sm *RunStateMachine) CanTransition(to domain.RunStatus) bool {
	return sm.machine.CanTransition(State(to))
}

// ValidTransitions returns all valid next states
func (sm *RunStateMachine) ValidTransitions() []domain.RunStatus {
	valid := sm.machine.ValidTransitions()
	result := make([]domain.RunStatus, len(valid))
	for i, s := range valid {
		result[i] = domain.RunStatus(s)
	}
	return result
}

// StepStateMachine manages the lifecycle of a Step
type StepStateMachine struct {
	machine *Machine
}

// NewStepStateMachine creates a new Step state machine
func NewStepStateMachine() *StepStateMachine {
	m := NewMachine(State(domain.StepPending))

	m.AddState(State(domain.StepReady))
	m.AddState(State(domain.StepRunning))
	m.AddState(State(domain.StepRetrying))
	m.AddState(State(domain.StepBlocked))
	m.AddState(State(domain.StepDone))
	m.AddState(State(domain.StepFailed))

	m.AddTransition(State(domain.StepPending), State(domain.StepReady), nil)
	m.AddTransition(State(domain.StepReady), State(domain.StepRunning), nil)
	m.AddTransition(State(domain.StepRunning), State(domain.StepDone), nil)
	m.AddTransition(State(domain.StepRunning), State(domain.StepFailed), nil)
	m.AddTransition(State(domain.StepRunning), State(domain.StepBlocked), nil)
	m.AddTransition(State(domain.StepFailed), State(domain.StepRetrying), nil)
	m.AddTransition(State(domain.StepRetrying), State(domain.StepRunning), nil)
	m.AddTransition(State(domain.StepBlocked), State(domain.StepRunning), nil)
	m.AddTransition(State(domain.StepBlocked), State(domain.StepFailed), nil)

	return &StepStateMachine{machine: m}
}

// Current returns the current state
func (sm *StepStateMachine) Current() domain.StepStatus {
	return domain.StepStatus(sm.machine.Current())
}

// SetCurrent sets the current state from a StepStatus
func (sm *StepStateMachine) SetCurrent(status domain.StepStatus) {
	sm.machine.SetCurrent(State(status))
}

// Transition attempts to transition to a new state
func (sm *StepStateMachine) Transition(to domain.StepStatus, context interface{}) error {
	if !to.IsValid() {
		return fmt.Errorf("invalid step status: %s", to)
	}
	return sm.machine.Transition(State(to), context)
}

// CanTransition checks if a transition is valid
func (sm *StepStateMachine) CanTransition(to domain.StepStatus) bool {
	return sm.machine.CanTransition(State(to))
}

// ValidTransitions returns all valid next states
func (sm *StepStateMachine) ValidTransitions() []domain.StepStatus {
	valid := sm.machine.ValidTransitions()
	result := make([]domain.StepStatus, len(valid))
	for i, s := range valid {
		result[i] = domain.StepStatus(s)
	}
	return result
}
