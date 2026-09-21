package main

import "testing"

func TestIsWatchableStatus(t *testing.T) {
	for _, s := range []string{
		"created", "planning", "plan_review", "awaiting_approval", "executing", "paused",
	} {
		if !isWatchableStatus(s) {
			t.Fatalf("%s should be watchable", s)
		}
	}
	for _, s := range []string{"completed", "failed", "cancelled"} {
		if isWatchableStatus(s) {
			t.Fatalf("%s should not be watchable", s)
		}
	}
}
