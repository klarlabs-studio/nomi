package main

import "testing"

func TestPendingSummaryZero(t *testing.T) {
	var p pendingSummary
	if p.PlanReviews != 0 || p.ToolApprovals != 0 || p.ActiveRuns != 0 {
		t.Fatalf("zero value should be empty: %+v", p)
	}
}
