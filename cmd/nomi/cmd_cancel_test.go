package main

import (
	"strings"
	"testing"
)

func TestIsCancelableStatus(t *testing.T) {
	for _, s := range []string{
		"created", "planning", "plan_review", "awaiting_approval", "executing", "paused",
	} {
		if !isCancelableStatus(s) {
			t.Fatalf("%s should be cancelable", s)
		}
	}
	for _, s := range []string{"completed", "failed", "cancelled"} {
		if isCancelableStatus(s) {
			t.Fatalf("%s should not be cancelable", s)
		}
	}
}

func TestCancelableFilter(t *testing.T) {
	rows := []runListRow{
		{ID: "1", Status: "executing"},
		{ID: "2", Status: "completed"},
		{ID: "3", Status: "plan_review"},
		{ID: "4", Status: "failed"},
	}
	var out []runListRow
	for _, r := range rows {
		if isCancelableStatus(r.Status) {
			out = append(out, r)
		}
	}
	if len(out) != 2 {
		t.Fatalf("got %d", len(out))
	}
	if !strings.Contains(out[0].Status+out[1].Status, "executing") {
		t.Fatalf("%v", out)
	}
}
