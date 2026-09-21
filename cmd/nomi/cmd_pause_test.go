package main

import "testing"

func TestIsPausableStatus(t *testing.T) {
	for _, s := range []string{"executing", "awaiting_approval"} {
		if !isPausableStatus(s) {
			t.Fatalf("%s should be pausable", s)
		}
	}
	for _, s := range []string{"created", "planning", "plan_review", "paused", "completed", "failed", "cancelled"} {
		if isPausableStatus(s) {
			t.Fatalf("%s should not be pausable", s)
		}
	}
}

func TestIsPausedStatus(t *testing.T) {
	if !isPausedStatus("paused") {
		t.Fatal("paused should match")
	}
	if isPausedStatus("executing") {
		t.Fatal("executing should not match paused")
	}
}
