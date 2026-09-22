package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPrioritizeRuns(t *testing.T) {
	in := []runListRow{
		{ID: "1", Status: "completed", Goal: "done"},
		{ID: "2", Status: "plan_review", Goal: "review me"},
		{ID: "3", Status: "executing", Goal: "go"},
		{ID: "4", Status: "failed", Goal: "nope"},
	}
	out := prioritizeRuns(in, 40)
	if len(out) != 4 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].ID != "2" || out[1].ID != "3" {
		t.Fatalf("active not first: %+v", out)
	}
}

func TestTUIModel_TabAndCursor(t *testing.T) {
	m := newTUIModel(&Client{URL: "http://example"})
	m.snap.Runs = []runListRow{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	m.snap.Approvals = []approvalRow{{ID: "x"}, {ID: "y"}}
	m.snap.Reviews = []runListRow{{ID: "r1"}}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = next.(tuiModel)
	if m.cursor != 1 {
		t.Fatalf("cursor=%d", m.cursor)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = next.(tuiModel)
	if m.tab != tabApprovals || m.cursor != 0 {
		t.Fatalf("tab=%v cursor=%d", m.tab, m.cursor)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = next.(tuiModel)
	if m.cursor != 1 {
		t.Fatalf("approval cursor=%d", m.cursor)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = next.(tuiModel)
	if m.cursor != 1 { // clamped
		t.Fatalf("should clamp to 1, got %d", m.cursor)
	}
}

func TestLoadTUISnapshot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.2.55"})
	})
	mux.HandleFunc("/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runs": []map[string]string{
				{"id": "run-1", "status": "plan_review", "goal": "fix it"},
				{"id": "run-2", "status": "completed", "goal": "old"},
			},
		})
	})
	mux.HandleFunc("/approvals", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"approvals": []map[string]any{
				{"id": "ap-1", "run_id": "run-1", "status": "pending", "capability": "filesystem.write"},
			},
		})
	})
	mux.HandleFunc("/schedules", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"schedules": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := &Client{URL: srv.URL, HTTP: srv.Client()}
	snap := loadTUISnapshot(cli)
	if snap.Health != "ok" || snap.Version != "0.2.55" {
		t.Fatalf("%+v", snap)
	}
	if len(snap.Runs) != 2 || snap.Runs[0].Status != "plan_review" {
		t.Fatalf("runs: %+v", snap.Runs)
	}
	if len(snap.Approvals) != 1 || snap.Pending.ToolApprovals != 1 {
		t.Fatalf("approvals: %+v pending=%+v", snap.Approvals, snap.Pending)
	}
	if snap.Pending.PlanReviews != 1 {
		t.Fatalf("plan reviews pending=%d", snap.Pending.PlanReviews)
	}
	if time.Since(snap.At) > time.Minute {
		t.Fatalf("At=%v", snap.At)
	}
}

func TestTUIModel_ApproveApproval(t *testing.T) {
	var resolved bool
	mux := http.NewServeMux()
	mux.HandleFunc("/approvals/ap-1/resolve", func(w http.ResponseWriter, r *http.Request) {
		resolved = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := newTUIModel(&Client{URL: srv.URL, HTTP: srv.Client()})
	m.tab = tabApprovals
	m.snap.Approvals = []approvalRow{{ID: "ap-1", Capability: "filesystem.write", Status: "pending"}}
	cmd := m.cmdApprove(true)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	done, ok := msg.(actionDoneMsg)
	if !ok || done.err != "" {
		t.Fatalf("%T %+v", msg, msg)
	}
	if !resolved {
		t.Fatal("resolve not called")
	}
	if done.ok == "" {
		t.Fatal("empty ok")
	}
}

func TestTUIViewContainsChrome(t *testing.T) {
	m := newTUIModel(&Client{URL: "http://127.0.0.1:8080"})
	m.snap.Health = "ok"
	m.snap.Version = "9.9.9"
	view := m.View()
	for _, want := range []string{"nomi tui", "http://127.0.0.1:8080", "1:Runs", "q quit"} {
		if !containsString(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}
