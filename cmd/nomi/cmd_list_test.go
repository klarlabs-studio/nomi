package main

import (
	"strings"
	"testing"
	"time"
)

func TestFormatWhen_Future(t *testing.T) {
	ts := time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)
	got := formatWhen(ts)
	if !strings.HasPrefix(got, "in ") {
		t.Fatalf("expected future phrasing, got %q", got)
	}
}

func TestFormatWhen_PastFallsBackToAgo(t *testing.T) {
	ts := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	got := formatWhen(ts)
	if !strings.Contains(got, "ago") {
		t.Fatalf("expected ago phrasing, got %q", got)
	}
}
