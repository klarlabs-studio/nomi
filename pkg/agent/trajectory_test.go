package agent_test

import (
	"testing"

	"github.com/felixgeelhaar/nomi/pkg/agent"
)

func TestTrajectoryHashChainVerifies(t *testing.T) {
	var tr agent.Trajectory
	tr = tr.Append("s1", "tax.assess", agent.StatusExecuted, "ok")
	tr = tr.Append("s2", "document.delete", agent.StatusDenied, "policy denied")
	tr = tr.Append("s3", "record.write", agent.StatusExecuted, "ok")

	outcomes := tr.Outcomes()
	if len(outcomes) != 3 {
		t.Fatalf("outcomes = %d, want 3", len(outcomes))
	}
	if outcomes[0].PrevHash != "" {
		t.Error("first PrevHash should be empty")
	}
	if outcomes[1].PrevHash != outcomes[0].Hash || outcomes[2].PrevHash != outcomes[1].Hash {
		t.Error("hash chain not linked")
	}
	if !tr.Verify() {
		t.Error("untampered trajectory should verify")
	}
}

func TestTrajectoryDetectsTampering(t *testing.T) {
	var tr agent.Trajectory
	tr = tr.Append("s1", "tax.assess", agent.StatusExecuted, "ok")
	tr = tr.Append("s2", "tax.assess", agent.StatusExecuted, "ok")

	outcomes := tr.Outcomes()
	outcomes[0].Status = agent.StatusDenied // tamper, leave stored hash intact
	if agent.TrajectoryFrom(outcomes).Verify() {
		t.Error("tampered trajectory must fail Verify")
	}
}

func TestTrajectoryEmptyVerifies(t *testing.T) {
	var tr agent.Trajectory
	if !tr.Verify() || tr.Len() != 0 {
		t.Error("empty trajectory should verify and be length 0")
	}
}
