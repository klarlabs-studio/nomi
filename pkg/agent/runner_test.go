package agent_test

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/nomi/pkg/agent"
)

func plan(steps ...agent.Step) agent.Plan { return agent.Plan{Goal: "test", Steps: steps} }

func TestRunnerGatesByPolicy(t *testing.T) {
	p := plan(
		agent.Step{ID: "s1", Capability: "tax.assess"},
		agent.Step{ID: "s2", Capability: "document.delete"}, // not allowed
	)
	policy := agent.AllowOnly("tax.assess")

	var executed []string
	runner := agent.NewRunner(func(_ context.Context, s agent.Step) error {
		executed = append(executed, s.ID)
		return nil
	}, nil)

	tr := runner.Run(context.Background(), p, policy)

	if len(executed) != 1 || executed[0] != "s1" {
		t.Errorf("executed = %v, want [s1] only", executed)
	}
	out := tr.Outcomes()
	if out[0].Status != agent.StatusExecuted || out[1].Status != agent.StatusDenied {
		t.Errorf("statuses = %s/%s, want executed/denied", out[0].Status, out[1].Status)
	}
	if !tr.Verify() {
		t.Error("trajectory should verify")
	}
}

func TestRunnerWildcardPolicy(t *testing.T) {
	// Reuses Nomi's wildcard permission engine: network.* allows network.outgoing.
	p := plan(agent.Step{ID: "s1", Capability: "network.outgoing"})
	policy := agent.Policy{Rules: []agent.Rule{{Capability: "network.*", Mode: agent.ModeAllow}}}

	tr := agent.NewRunner(func(context.Context, agent.Step) error { return nil }, nil).
		Run(context.Background(), p, policy)
	if tr.Outcomes()[0].Status != agent.StatusExecuted {
		t.Errorf("wildcard allow failed: %s", tr.Outcomes()[0].Status)
	}
}

func TestRunnerConfirmRequiresApprover(t *testing.T) {
	p := plan(agent.Step{ID: "s1", Capability: "filesystem.write"})
	policy := agent.Policy{Rules: []agent.Rule{{Capability: "filesystem.write", Mode: agent.ModeConfirm}}}

	// No approver: confirm-mode step is gated.
	tr := agent.NewRunner(func(context.Context, agent.Step) error { return nil }, nil).
		Run(context.Background(), p, policy)
	if tr.Outcomes()[0].Status != agent.StatusConfirmRequired {
		t.Errorf("status = %s, want confirm_required", tr.Outcomes()[0].Status)
	}

	// Approver approves: step executes.
	tr = agent.NewRunner(
		func(context.Context, agent.Step) error { return nil },
		func(context.Context, agent.Step) bool { return true },
	).Run(context.Background(), p, policy)
	if tr.Outcomes()[0].Status != agent.StatusExecuted {
		t.Errorf("status = %s, want executed after approval", tr.Outcomes()[0].Status)
	}
}

func TestRunnerRecordsFailure(t *testing.T) {
	p := plan(agent.Step{ID: "s1", Capability: "tax.assess"})
	tr := agent.NewRunner(func(context.Context, agent.Step) error { return errors.New("boom") }, nil).
		Run(context.Background(), p, agent.AllowOnly("tax.assess"))
	if tr.Outcomes()[0].Status != agent.StatusFailed {
		t.Errorf("status = %s, want failed", tr.Outcomes()[0].Status)
	}
}
