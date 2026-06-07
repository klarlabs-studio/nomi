package github

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/plugins"
)

func TestParseRepoAllowlist(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"acme/widgets", 1},
		{"acme/widgets, acme/gadgets", 2},
		{"  acme/widgets  ", 1},
		{"acme/", 0},    // missing repo
		{"/widgets", 0}, // missing owner
		{"acme", 0},     // missing slash
		{"", 0},
	}
	for _, tc := range cases {
		got := parseRepoAllowlist(map[string]any{configRepoAllowlist: tc.input})
		if len(got) != tc.want {
			t.Errorf("input %q: got %d entries, want %d (%+v)", tc.input, len(got), tc.want, got)
		}
	}
}

func TestPollingEnabled_StringAndBool(t *testing.T) {
	cases := []struct {
		input any
		want  bool
	}{
		{nil, false},
		{true, true},
		{false, false},
		{"true", true},
		{"TRUE", true},
		{"false", false},
		{"yes", false}, // strict
		{42, false},    // unsupported type
	}
	for _, tc := range cases {
		got := pollingEnabled(map[string]any{configPollingEnabled: tc.input})
		if got != tc.want {
			t.Errorf("input %v: got %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestGitHubTrigger_FirstPollEstablishesBaseline(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/issues",
		http.StatusOK,
		`[{"number":3,"title":"newest","state":"open"},{"number":2,"title":"middle","state":"open"},{"number":1,"title":"oldest","state":"open"}]`)
	p, conn := stubPlugin(t, srv)

	auth, err := p.authClientFor(conn)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	tr := &githubTrigger{
		plugin:             p,
		connectionID:       conn.ID,
		kind:               TriggerKindIssueOpened,
		repos:              []repoRef{{"o", "r"}},
		authClient:         auth,
		installation:       555,
		stop:               make(chan struct{}),
		highestIssueNumber: map[string]int{},
		seenReviewRequests: map[string]map[int]bool{},
	}

	var fired []plugins.TriggerEvent
	var mu sync.Mutex
	cb := func(_ context.Context, ev plugins.TriggerEvent) error {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
		return nil
	}

	tr.poll(context.Background(), cb)
	mu.Lock()
	if len(fired) != 0 {
		t.Errorf("first poll should establish baseline without firing, got %d events", len(fired))
	}
	mu.Unlock()
	if got := tr.highestIssueNumber["o/r"]; got != 3 {
		t.Fatalf("baseline highest = %d, want 3", got)
	}
}

func TestGitHubTrigger_SubsequentPollFiresOnNewIssues(t *testing.T) {
	srv := newStubServer(t)
	// First poll: 3 issues. Second poll: 5 issues (#4, #5 are new).
	srv.stub("GET /repos/o/r/issues",
		http.StatusOK,
		`[{"number":3,"title":"three","state":"open"},{"number":2,"title":"two","state":"open"},{"number":1,"title":"one","state":"open"}]`)
	p, conn := stubPlugin(t, srv)
	auth, _ := p.authClientFor(conn)
	tr := &githubTrigger{
		plugin: p, connectionID: conn.ID, kind: TriggerKindIssueOpened,
		repos: []repoRef{{"o", "r"}}, authClient: auth, installation: 555,
		stop: make(chan struct{}), highestIssueNumber: map[string]int{}, seenReviewRequests: map[string]map[int]bool{},
	}
	cb := func(_ context.Context, _ plugins.TriggerEvent) error { return nil }
	// First poll → baseline=3.
	tr.poll(context.Background(), cb)

	// Second poll returns #4 and #5 in addition.
	srv.stub("GET /repos/o/r/issues",
		http.StatusOK,
		`[{"number":5,"title":"five","state":"open"},{"number":4,"title":"four","state":"open"},{"number":3,"title":"three","state":"open"},{"number":2,"title":"two","state":"open"},{"number":1,"title":"one","state":"open"}]`)
	var fired []plugins.TriggerEvent
	cb2 := func(_ context.Context, ev plugins.TriggerEvent) error {
		fired = append(fired, ev)
		return nil
	}
	tr.poll(context.Background(), cb2)
	if len(fired) != 2 {
		t.Fatalf("expected 2 fires (#4, #5), got %d: %+v", len(fired), fired)
	}
	// Order: smaller-number issue fires first (we iterate in reverse
	// over the desc-sorted response).
	want := []int{4, 5}
	for i, ev := range fired {
		got, _ := ev.Metadata["issue_number"].(int)
		if got != want[i] {
			t.Errorf("fired[%d]: issue_number = %d, want %d", i, got, want[i])
		}
	}
}

func TestGitHubTrigger_StartRefusesEmptyAllowlist(t *testing.T) {
	tr := &githubTrigger{
		repos: nil, // no repos
		stop:  make(chan struct{}),
	}
	err := tr.Start(context.Background(), func(_ context.Context, _ plugins.TriggerEvent) error { return nil })
	if err == nil {
		t.Fatal("expected error on empty allowlist")
	}
}

func TestGitHubTrigger_StopCancelsLoop(t *testing.T) {
	tr := &githubTrigger{
		repos:              []repoRef{{"o", "r"}},
		stop:               make(chan struct{}),
		highestIssueNumber: map[string]int{},
		seenReviewRequests: map[string]map[int]bool{},
	}
	// Start with a no-op callback. The Start path normally validates
	// auth — bypass by skipping the validation and only testing that
	// Stop() is idempotent and returns nil.
	if err := tr.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	// Second Stop must not panic on closed channel.
	if err := tr.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	// Channel must be closed.
	select {
	case <-tr.stop:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("stop channel not closed")
	}
}
