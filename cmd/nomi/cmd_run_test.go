package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintStepProgress_StatusTransitions(t *testing.T) {
	plan := &planPayload{Steps: []planStep{
		{ID: "s1", Title: "Read README", ExpectedTool: "filesystem.read"},
		{ID: "s2", Title: "Patch", ExpectedTool: "filesystem.patch"},
	}}
	seen := map[string]string{}
	var buf bytes.Buffer

	printStepProgress(&buf, []stepProgressRow{
		{ID: "s1", Title: "Read README", Status: "running"},
	}, plan, seen)
	if !strings.Contains(buf.String(), "→ Read README (filesystem.read)") {
		t.Fatalf("running: %q", buf.String())
	}

	buf.Reset()
	printStepProgress(&buf, []stepProgressRow{
		{ID: "s1", Title: "Read README", Status: "running"},
	}, plan, seen)
	if buf.Len() != 0 {
		t.Fatalf("duplicate running should be silent: %q", buf.String())
	}

	buf.Reset()
	printStepProgress(&buf, []stepProgressRow{
		{ID: "s1", Title: "Read README", Status: "done"},
		{ID: "s2", Title: "Patch", Status: "failed", Error: "conflict"},
	}, plan, seen)
	out := buf.String()
	if !strings.Contains(out, "✓ Read README (filesystem.read)") {
		t.Fatalf("done missing: %q", out)
	}
	if !strings.Contains(out, "✗ Patch (filesystem.patch): conflict") {
		t.Fatalf("failed missing: %q", out)
	}
}

func TestPrintStepProgress_RetryAndBlocked(t *testing.T) {
	seen := map[string]string{}
	var buf bytes.Buffer
	printStepProgress(&buf, []stepProgressRow{
		{ID: "a", Title: "Shell", Status: "retrying"},
		{ID: "b", Title: "Write", Status: "blocked"},
	}, nil, seen)
	out := buf.String()
	if !strings.Contains(out, "↻ Shell (retry)") {
		t.Fatalf("retry: %q", out)
	}
	if !strings.Contains(out, "⏸ Write (blocked)") {
		t.Fatalf("blocked: %q", out)
	}
}
