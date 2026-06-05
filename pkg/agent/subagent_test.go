package agent_test

import (
	"context"
	"testing"

	"github.com/felixgeelhaar/nomi/pkg/agent"
)

func TestRunWithSubagentsSpawnsGatedChild(t *testing.T) {
	runner := agent.NewRunner(func(context.Context, agent.Step) error { return nil }, nil)

	parent := agent.Plan{Goal: "orchestrate", Steps: []agent.Step{
		{ID: "spawn", Capability: "agent.spawn"},
	}}
	// Child has its own policy: it may classify but not delete.
	child := agent.Subagent{
		ParentStepID: "spawn",
		Plan: agent.Plan{Goal: "child", Steps: []agent.Step{
			{ID: "c1", Capability: "document.classify"},
			{ID: "c2", Capability: "document.delete"},
		}},
		Policy: agent.AllowOnly("document.classify"),
	}

	result := runner.RunWithSubagents(context.Background(), parent,
		agent.AllowOnly("agent.spawn"), []agent.Subagent{child})

	if result.Trajectory.Outcomes()[0].Status != agent.StatusExecuted {
		t.Fatal("parent spawn step should execute")
	}
	if len(result.Children) != 1 {
		t.Fatalf("children = %d, want 1", len(result.Children))
	}
	childOut := result.Children[0].Trajectory.Outcomes()
	if childOut[0].Status != agent.StatusExecuted || childOut[1].Status != agent.StatusDenied {
		t.Errorf("child gated independently expected executed/denied, got %s/%s",
			childOut[0].Status, childOut[1].Status)
	}
	// Whole tree's trajectories all verify.
	for _, tr := range result.AllTrajectories() {
		if !tr.Verify() {
			t.Error("a trajectory in the tree failed to verify")
		}
	}
}

func TestSubagentNotSpawnedWhenParentStepDenied(t *testing.T) {
	runner := agent.NewRunner(func(context.Context, agent.Step) error { return nil }, nil)
	parent := agent.Plan{Steps: []agent.Step{{ID: "spawn", Capability: "agent.spawn"}}}
	child := agent.Subagent{
		ParentStepID: "spawn",
		Plan:         agent.Plan{Steps: []agent.Step{{ID: "c1", Capability: "document.classify"}}},
		Policy:       agent.AllowOnly("document.classify"),
	}

	// Parent policy denies agent.spawn -> the spawn step never runs, so no child.
	result := runner.RunWithSubagents(context.Background(), parent,
		agent.AllowOnly("something.else"), []agent.Subagent{child})

	if result.Trajectory.Outcomes()[0].Status != agent.StatusDenied {
		t.Fatal("spawn step should be denied")
	}
	if len(result.Children) != 0 {
		t.Errorf("children = %d, want 0 (parent denied)", len(result.Children))
	}
}
