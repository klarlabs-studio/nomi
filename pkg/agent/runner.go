package agent

import "context"

// Step is a single planned action. It declares the capability it requires; the
// runner will not execute it unless the policy allows that capability.
type Step struct {
	ID         string
	Title      string
	Capability string
	Arguments  map[string]any
}

// Plan is an ordered sequence of steps proposed for execution. It is the
// reviewable unit (plan-review-before-execution).
type Plan struct {
	Goal  string
	Steps []Step
}

// Executor performs the side-effecting work of an allowed step (e.g. invoking a
// tool). Supplied by the caller so the runner stays free of infrastructure.
type Executor func(ctx context.Context, step Step) error

// Approver decides whether a confirm-mode step may proceed. Optional; when nil,
// confirm-mode steps are not executed and recorded as confirm_required.
type Approver func(ctx context.Context, step Step) bool

// Runner executes plans under capability gating, recording a Trajectory.
type Runner struct {
	exec     Executor
	approver Approver
}

// NewRunner builds a Runner with the given executor and optional approver.
func NewRunner(exec Executor, approver Approver) *Runner {
	return &Runner{exec: exec, approver: approver}
}

// Run executes the plan's steps in order, gating each on the policy:
//   - allow: the executor runs; recorded executed or failed.
//   - confirm: executed only if an approver approves, else confirm_required.
//   - deny: never executed; recorded denied.
//
// The returned Trajectory is the auditable record of the run.
func (r *Runner) Run(ctx context.Context, plan Plan, policy Policy) Trajectory {
	var tr Trajectory
	for _, step := range plan.Steps {
		cap := step.Capability
		switch policy.evaluate(cap) {
		case ModeAllow:
			tr = r.execute(ctx, tr, step)
		case ModeConfirm:
			if r.approver != nil && r.approver(ctx, step) {
				tr = r.execute(ctx, tr, step)
			} else {
				tr = tr.Append(step.ID, cap, StatusConfirmRequired, "confirmation required for "+cap)
			}
		default: // ModeDeny and any unknown mode
			tr = tr.Append(step.ID, cap, StatusDenied, "policy denied "+cap)
		}
	}
	return tr
}

func (r *Runner) execute(ctx context.Context, tr Trajectory, step Step) Trajectory {
	if err := r.exec(ctx, step); err != nil {
		return tr.Append(step.ID, step.Capability, StatusFailed, err.Error())
	}
	return tr.Append(step.ID, step.Capability, StatusExecuted, "ok")
}
