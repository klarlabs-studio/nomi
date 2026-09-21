package main

import (
	"strings"
	"testing"
)

func TestResolveApprovalID(t *testing.T) {
	pending := []approvalRow{
		{ID: "aaaaaaaa-1111-2222-3333-444444444444", Status: "pending", Capability: "filesystem.write"},
		{ID: "bbbbbbbb-1111-2222-3333-444444444444", Status: "pending", Capability: "command.exec"},
	}

	id, err := resolveApprovalID("aaaaaaaa", pending)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "aaaaaaaa") {
		t.Fatalf("got %s", id)
	}

	_, err = resolveApprovalID("aaaa", []approvalRow{
		{ID: "aaaa1111", Status: "pending"},
		{ID: "aaaa2222", Status: "pending"},
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous, got %v", err)
	}

	_, err = resolveApprovalID("ffffffff", pending)
	if err == nil {
		t.Fatal("expected not found")
	}

	_, err = resolveApprovalID("", pending)
	if err == nil {
		t.Fatal("empty hint should error")
	}
}

func TestListPendingApprovalsFilter(t *testing.T) {
	rows := []approvalRow{
		{ID: "1", Status: "pending"},
		{ID: "2", Status: "approved"},
		{ID: "3", Status: "pending"},
	}
	var out []approvalRow
	for _, a := range rows {
		if a.Status == "pending" {
			out = append(out, a)
		}
	}
	if len(out) != 2 {
		t.Fatalf("got %d", len(out))
	}
}
