package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPullsList_Basic(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/pulls", http.StatusOK,
		`[{"number":1,"title":"PR one","state":"open","user":{"login":"alice"},"head":{"sha":"abc","ref":"feat/x"},"base":{"ref":"main"}}]`)
	p, conn := stubPlugin(t, srv)
	out, err := p.pullsList(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
	})
	if err != nil {
		t.Fatalf("pullsList: %v", err)
	}
	pulls, _ := out["pulls"].([]map[string]any)
	if len(pulls) != 1 || pulls[0]["title"] != "PR one" {
		t.Fatalf("unexpected pulls shape: %+v", pulls)
	}
	if pulls[0]["head_sha"] != "abc" || pulls[0]["base_ref"] != "main" {
		t.Fatalf("expected nested head/base flattening: %+v", pulls[0])
	}
}

func TestPullsGet_BundlesCheckRuns(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/pulls/7", http.StatusOK,
		`{"number":7,"title":"X","state":"open","body":"Fixes #42 also closes #43","user":{"login":"alice"},"head":{"sha":"deadbeef","ref":"feat"},"base":{"ref":"main"}}`)
	srv.stub("GET /repos/o/r/commits/deadbeef/check-runs", http.StatusOK,
		`{"total_count":2,"check_runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"tests","status":"in_progress"}]}`)
	p, conn := stubPlugin(t, srv)
	out, err := p.pullsGet(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "pull_number": float64(7),
	})
	if err != nil {
		t.Fatalf("pullsGet: %v", err)
	}
	checks, _ := out["check_runs"].([]map[string]any)
	if len(checks) != 2 {
		t.Fatalf("expected 2 check_runs, got %d", len(checks))
	}
	links, _ := out["linked_issues"].([]string)
	wantSeen := map[string]bool{"#42": false, "#43": false}
	for _, l := range links {
		if _, ok := wantSeen[l]; ok {
			wantSeen[l] = true
		}
	}
	for k, v := range wantSeen {
		if !v {
			t.Fatalf("missing linked issue %s in %v", k, links)
		}
	}
}

func TestPullsGet_TolerantOfCheckRunsFailure(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/pulls/7", http.StatusOK,
		`{"number":7,"title":"X","head":{"sha":"deadbeef"},"base":{"ref":"main"}}`)
	// No stub for check-runs path → 404 from the stub server. The
	// pullsGet handler should still succeed with check_runs_error
	// recorded.
	p, conn := stubPlugin(t, srv)
	out, err := p.pullsGet(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "pull_number": float64(7),
	})
	if err != nil {
		t.Fatalf("pullsGet should not propagate check-runs failure: %v", err)
	}
	pull, _ := out["pull"].(map[string]any)
	if _, ok := pull["check_runs_error"]; !ok {
		t.Fatalf("expected check_runs_error field, got %+v", pull)
	}
}

func TestPullsReview_RejectsUnknownEvent(t *testing.T) {
	srv := newStubServer(t)
	p, conn := stubPlugin(t, srv)
	_, err := p.pullsReview(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"pull_number": float64(7), "event": "MERGE",
	})
	if err == nil || !strings.Contains(err.Error(), "event must be") {
		t.Fatalf("expected validation error on unknown event, got %v", err)
	}
}

func TestPullsReview_RejectsEmptyCommentBody(t *testing.T) {
	srv := newStubServer(t)
	p, conn := stubPlugin(t, srv)
	_, err := p.pullsReview(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"pull_number": float64(7), "event": "comment", "body": "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "non-empty body") {
		t.Fatalf("expected non-empty-body error, got %v", err)
	}
}

func TestPullsReview_AcceptsApproveWithoutBody(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("POST /repos/o/r/pulls/7/reviews", http.StatusOK,
		`{"id":1,"state":"APPROVED","user":{"login":"bot"}}`)
	p, conn := stubPlugin(t, srv)
	_, err := p.pullsReview(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"pull_number": float64(7), "event": "APPROVE",
	})
	if err != nil {
		t.Fatalf("APPROVE without body should be allowed: %v", err)
	}
}

func TestExtractIssueRefs(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"fixes #42", []string{"#42"}},
		{"closes #1, also closes #2", []string{"#1", "#2"}},
		{"unrelated 123 and #456", nil},
		{"", nil},
		{"Resolves #100\nFixes #200", []string{"#100", "#200"}},
	}
	for _, tc := range cases {
		got := extractIssueRefs(tc.body)
		if len(got) != len(tc.want) {
			t.Errorf("body=%q got %v want %v", tc.body, got, tc.want)
			continue
		}
		seen := map[string]bool{}
		for _, g := range got {
			seen[g] = true
		}
		for _, w := range tc.want {
			if !seen[w] {
				t.Errorf("body=%q missing %q in %v", tc.body, w, got)
			}
		}
	}
}

func TestPullsCreate_PassesPayload(t *testing.T) {
	srv := newStubServer(t)
	var seenBody string
	srv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/access_tokens") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_stub","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/repos/o/r/pulls" {
			b, _ := readAll(r.Body)
			seenBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":12,"title":"Add feature","state":"open","head":{"sha":"a","ref":"feat"},"base":{"ref":"main"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	p, conn := stubPlugin(t, srv)
	out, err := p.pullsCreate(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"title": "Add feature", "head": "feat", "base": "main",
		"body": "explains the change", "draft": true,
	})
	if err != nil {
		t.Fatalf("pullsCreate: %v", err)
	}
	for _, want := range []string{`"title":"Add feature"`, `"head":"feat"`, `"base":"main"`, `"body":"explains the change"`, `"draft":true`} {
		if !strings.Contains(seenBody, want) {
			t.Errorf("payload missing %s; got %s", want, seenBody)
		}
	}
	pull, _ := out["pull"].(map[string]any)
	if pull["title"] != "Add feature" {
		t.Fatalf("trimmed pull missing title: %+v", pull)
	}
}

func TestPullsCreate_RequiresFields(t *testing.T) {
	p, conn := stubPlugin(t, newStubServer(t))
	cases := []map[string]any{
		{"connection_id": "c1", "owner": "o", "repo": "r", "head": "h", "base": "b"},  // no title
		{"connection_id": "c1", "owner": "o", "repo": "r", "title": "t", "base": "b"}, // no head
		{"connection_id": "c1", "owner": "o", "repo": "r", "title": "t", "head": "h"}, // no base
		{"connection_id": "c1", "repo": "r", "title": "t", "head": "h", "base": "b"},  // no owner
	}
	for i, in := range cases {
		if _, err := p.pullsCreate(context.Background(), conn, in); err == nil {
			t.Fatalf("case %d: expected error for incomplete input %v", i, in)
		}
	}
}

func TestPullsCreate_RespectsRepoAllowlist(t *testing.T) {
	srv := newStubServer(t)
	p, conn := stubPlugin(t, srv)
	conn.Config[configRepoAllowlist] = "permitted/repo"

	if _, err := p.pullsCreate(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "blocked", "repo": "repo",
		"title": "t", "head": "h", "base": "b",
	}); err == nil {
		t.Fatal("expected allowlist refusal")
	}
}
