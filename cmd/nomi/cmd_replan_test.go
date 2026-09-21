package main

import "testing"

func TestIsReplanableStatus(t *testing.T) {
	for _, s := range []string{"failed", "cancelled"} {
		if !isReplanableStatus(s) {
			t.Fatalf("%s should be replanable", s)
		}
	}
	for _, s := range []string{"executing", "plan_review", "completed", "paused"} {
		if isReplanableStatus(s) {
			t.Fatalf("%s should not match --list replanable filter", s)
		}
	}
}
