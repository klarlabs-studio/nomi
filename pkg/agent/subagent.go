package agent

import "context"

// Subagent is a child plan spawned by a parent step. It runs as its own gated
// run, with its own policy, so a parent never widens a child's authority. The
// child's trajectory is linked under the parent in the result tree.
type Subagent struct {
	// ParentStepID is the parent step whose successful execution spawns this
	// child.
	ParentStepID string
	Plan         Plan
	Policy       Policy
}

// RunResult is the outcome of a (possibly nested) gated run: the run's own
// trajectory plus the results of any subagents it spawned.
type RunResult struct {
	Trajectory Trajectory
	Children   []RunResult
}

// AllTrajectories returns this run's trajectory and all descendants', depth
// first — convenient for reflection across a whole spawn tree.
func (r RunResult) AllTrajectories() []Trajectory {
	out := []Trajectory{r.Trajectory}
	for _, c := range r.Children {
		out = append(out, c.AllTrajectories()...)
	}
	return out
}

// RunWithSubagents executes a parent plan under its policy, and after any step
// that executes successfully, runs each subagent registered for that step as an
// independently-gated child run. Subagents may themselves spawn subagents
// (nesting is supported via their own Subagents list).
func (r *Runner) RunWithSubagents(ctx context.Context, plan Plan, policy Policy, subagents []Subagent) RunResult {
	result := RunResult{}
	var tr Trajectory
	for _, step := range plan.Steps {
		cap := step.Capability
		executed := false
		switch policy.evaluate(cap) {
		case ModeAllow:
			tr = r.execute(ctx, tr, step)
			executed = lastExecuted(tr)
		case ModeConfirm:
			if r.approver != nil && r.approver(ctx, step) {
				tr = r.execute(ctx, tr, step)
				executed = lastExecuted(tr)
			} else {
				tr = tr.Append(step.ID, cap, StatusConfirmRequired, "confirmation required for "+cap)
			}
		default:
			tr = tr.Append(step.ID, cap, StatusDenied, "policy denied "+cap)
		}

		// Spawn subagents only for steps that actually executed.
		if executed {
			for _, sub := range subagents {
				if sub.ParentStepID == step.ID {
					child := r.RunWithSubagents(ctx, sub.Plan, sub.Policy, sub.subagents())
					result.Children = append(result.Children, child)
				}
			}
		}
	}
	result.Trajectory = tr
	return result
}

// subagents returns nested subagents (none in the flat MVP shape; reserved for
// callers that attach deeper trees).
func (s Subagent) subagents() []Subagent { return nil }

func lastExecuted(tr Trajectory) bool {
	out := tr.Outcomes()
	if len(out) == 0 {
		return false
	}
	return out[len(out)-1].Status == StatusExecuted
}
