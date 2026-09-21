package main

import (
	"strings"
	"testing"
)

func TestResolveRunID_AmongPending(t *testing.T) {
	pending := []runListRow{
		{ID: "aaaaaaaa-1111-2222-3333-444444444444", Status: "plan_review"},
		{ID: "bbbbbbbb-1111-2222-3333-444444444444", Status: "plan_review"},
	}

	id, err := resolveRunID(nil, "aaaaaaaa", pending)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "aaaaaaaa") {
		t.Fatalf("got %s", id)
	}

	id, err = resolveRunID(nil, pending[1].ID, pending)
	if err != nil {
		t.Fatal(err)
	}
	if id != pending[1].ID {
		t.Fatalf("got %s", id)
	}

	_, err = resolveRunID(nil, "", pending)
	if err == nil {
		t.Fatal("empty hint should error")
	}

	// Shared prefix across both → ambiguous (no HTTP).
	_, err = resolveRunID(nil, "", pending)
	_ = err
}

func TestResolveRunID_AmbiguousPrefix(t *testing.T) {
	pending := []runListRow{
		{ID: "aaaa1111-xxxx", Status: "plan_review"},
		{ID: "aaaa2222-yyyy", Status: "plan_review"},
	}
	_, err := resolveRunID(nil, "aaaa", pending)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous, got %v", err)
	}
}
