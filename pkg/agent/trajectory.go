// Package agent is Nomi's public agent-runtime library: gated plan execution
// with a tamper-evident trajectory, subagent spawning, and reflection. It
// reuses Nomi's canonical permission engine for capability gating while
// exposing a clean, self-contained public API that callers can embed without
// reaching into Nomi's internal packages.
//
// The control posture is "plan-review, gated execution": a reviewed plan's
// steps each declare a required capability, execution is gated on a policy, and
// every outcome is recorded in a hash-chained Trajectory for audit.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
)

// Status is the outcome of attempting a plan step.
type Status string

const (
	// StatusExecuted means the capability was allowed and the step ran.
	StatusExecuted Status = "executed"
	// StatusDenied means the policy denied the capability; the step did not run.
	StatusDenied Status = "denied"
	// StatusConfirmRequired means the policy requires confirmation and no
	// approver granted it; the step did not run.
	StatusConfirmRequired Status = "confirm_required"
	// StatusFailed means the capability was allowed but the executor errored.
	StatusFailed Status = "failed"
)

// Outcome is one tamper-evident link in a Trajectory. Each link hash-chains the
// previous link with this step's identity, required capability, and result.
type Outcome struct {
	StepID     string `json:"step_id"`
	Capability string `json:"capability"`
	Status     Status `json:"status"`
	Detail     string `json:"detail"`
	PrevHash   string `json:"prev_hash"`
	Hash       string `json:"hash"`
}

// Trajectory is the append-only, hash-chained record of a plan run. It is the
// agent audit trail: every step's outcome links to the previous so any later
// tampering is detectable.
type Trajectory struct {
	outcomes []Outcome
}

// Append records a new step outcome, chaining it to the prior hash, and returns
// the extended trajectory. Value semantics keep it append-only.
func (t Trajectory) Append(stepID, capability string, status Status, detail string) Trajectory {
	var prev string
	if n := len(t.outcomes); n > 0 {
		prev = t.outcomes[n-1].Hash
	}
	outcome := Outcome{
		StepID:     stepID,
		Capability: capability,
		Status:     status,
		Detail:     detail,
		PrevHash:   prev,
		Hash:       linkHash(prev, stepID, capability, status, detail),
	}
	next := make([]Outcome, len(t.outcomes), len(t.outcomes)+1)
	copy(next, t.outcomes)
	return Trajectory{outcomes: append(next, outcome)}
}

// Outcomes returns a copy of the recorded outcomes in order.
func (t Trajectory) Outcomes() []Outcome {
	cp := make([]Outcome, len(t.outcomes))
	copy(cp, t.outcomes)
	return cp
}

// Len reports the number of recorded steps.
func (t Trajectory) Len() int { return len(t.outcomes) }

// Verify recomputes the hash chain and reports whether it is intact. Any change
// to a recorded capability, status, detail, or ordering breaks verification.
func (t Trajectory) Verify() bool {
	var prev string
	for _, o := range t.outcomes {
		if o.PrevHash != prev {
			return false
		}
		if o.Hash != linkHash(prev, o.StepID, o.Capability, o.Status, o.Detail) {
			return false
		}
		prev = o.Hash
	}
	return true
}

// TrajectoryFrom rebuilds a Trajectory from persisted outcomes without
// recomputing hashes. Verify can then confirm the loaded chain is untampered.
func TrajectoryFrom(outcomes []Outcome) Trajectory {
	cp := make([]Outcome, len(outcomes))
	copy(cp, outcomes)
	return Trajectory{outcomes: cp}
}

func linkHash(prev, stepID, capability string, status Status, detail string) string {
	h := sha256.New()
	for _, part := range []string{prev, stepID, capability, string(status), detail} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
