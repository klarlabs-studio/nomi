package agent_test

import (
	"context"
	"testing"

	"go.klarlabs.de/nomi/pkg/agent"
)

func hasInsight(insights []agent.Insight, kind, capability string, count int) bool {
	for _, in := range insights {
		if in.Kind == kind && in.Capability == capability && in.Count == count {
			return true
		}
	}
	return false
}

func TestReflectSkillAndFriction(t *testing.T) {
	insights := agent.Reflect([]agent.Observation{
		{Capability: "tax.assess", Status: agent.StatusExecuted},
		{Capability: "tax.assess", Status: agent.StatusExecuted},
		{Capability: "document.delete", Status: agent.StatusDenied},
		{Capability: "document.delete", Status: agent.StatusDenied},
		{Capability: "once.only", Status: agent.StatusExecuted}, // below threshold
	})
	if !hasInsight(insights, agent.KindSkill, "tax.assess", 2) {
		t.Error("expected tax.assess skill (count 2)")
	}
	if !hasInsight(insights, agent.KindFriction, "document.delete", 2) {
		t.Error("expected document.delete friction (count 2)")
	}
	for _, in := range insights {
		if in.Capability == "once.only" {
			t.Error("once.only should be below the skill threshold")
		}
	}
}

func TestReflectOverTrajectories(t *testing.T) {
	runner := agent.NewRunner(func(context.Context, agent.Step) error { return nil }, nil)
	policy := agent.AllowOnly("tax.assess")
	var trs []agent.Trajectory
	for range 2 {
		tr := runner.Run(context.Background(), agent.Plan{Steps: []agent.Step{
			{ID: "a", Capability: "tax.assess"},
			{ID: "d", Capability: "document.delete"},
		}}, policy)
		trs = append(trs, tr)
	}
	insights := agent.Reflect(agent.ObservationsFromTrajectories(trs...))
	if !hasInsight(insights, agent.KindSkill, "tax.assess", 2) {
		t.Errorf("expected skill insight from trajectories; got %+v", insights)
	}
	if !hasInsight(insights, agent.KindFriction, "document.delete", 2) {
		t.Errorf("expected friction insight from trajectories; got %+v", insights)
	}
}

func TestReflectEmpty(t *testing.T) {
	if got := agent.Reflect(nil); len(got) != 0 {
		t.Errorf("Reflect(nil) = %+v, want empty", got)
	}
}
