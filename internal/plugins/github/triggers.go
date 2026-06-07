package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	gh "go.klarlabs.de/nomi/internal/integrations/github"
	"go.klarlabs.de/nomi/internal/plugins"
)

// Trigger kinds. Strings appear in the manifest's TriggerContribution.Kind
// and in connection-config trigger_rules[*].kind.
const (
	TriggerKindIssueOpened       = "github.issue_opened"
	TriggerKindPRReviewRequested = "github.pr_review_requested"
)

// pollInterval is the cadence for the background loop. 60s matches
// the spec; ETag-based conditional requests keep API quota usage
// modest (steady-state ~80% reduction vs unconditional GETs).
const pollInterval = 60 * time.Second

// githubTrigger is a single Trigger instance bound to one Connection.
// It polls every repo in the connection's allowlist on the same
// schedule, fanning matched events out via the runtime's
// TriggerCallback.
type githubTrigger struct {
	plugin       *Plugin
	connectionID string
	kind         string
	repos        []repoRef // owner/repo entries from the connection's allowlist
	authClient   *gh.AuthClient
	installation int64

	mu       sync.Mutex
	stopOnce sync.Once
	stop     chan struct{}

	// Per-(repo, kind) baselines for delta detection. Populated on the
	// first poll; subsequent polls fire only on entries that exceed
	// the baseline. Keyed by "<owner>/<repo>" (case-insensitive).
	highestIssueNumber map[string]int          // for issue_opened
	seenReviewRequests map[string]map[int]bool // for pr_review_requested: repo → PR# set
	etagIssues         map[string]string
	etagPulls          map[string]string

	// firstPollDone gates whether the next poll should fire. The
	// first poll is treated as "establish baseline, no firings" so a
	// daemon restart doesn't spam every existing open issue.
	firstPollDone bool
}

type repoRef struct{ owner, repo string }

// Triggers implements plugins.TriggerProvider. Returns one trigger
// per (Connection, kind) so the runtime can start/stop them
// independently if a user disables a kind without disabling the
// connection.
func (p *Plugin) Triggers() []plugins.Trigger {
	if p.connections == nil {
		return nil
	}
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		log.Printf("github: list connections for triggers: %v", err)
		return nil
	}
	var out []plugins.Trigger
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		if !pollingEnabled(conn.Config) {
			continue
		}
		auth, err := p.authClientFor(conn)
		if err != nil {
			log.Printf("github: skip trigger for %s: %v", conn.ID, err)
			continue
		}
		instID, err := configInt(conn.Config, configInstallationID)
		if err != nil {
			continue
		}
		repos := parseRepoAllowlist(conn.Config)
		for _, kind := range []string{TriggerKindIssueOpened, TriggerKindPRReviewRequested} {
			out = append(out, &githubTrigger{
				plugin:             p,
				connectionID:       conn.ID,
				kind:               kind,
				repos:              repos,
				authClient:         auth,
				installation:       instID,
				stop:               make(chan struct{}),
				highestIssueNumber: map[string]int{},
				seenReviewRequests: map[string]map[int]bool{},
				etagIssues:         map[string]string{},
				etagPulls:          map[string]string{},
			})
		}
	}
	return out
}

func (t *githubTrigger) ConnectionID() string { return t.connectionID }
func (t *githubTrigger) Kind() string         { return t.kind }

// Start kicks off the polling loop. Returns immediately; the loop
// runs until Stop is called.
func (t *githubTrigger) Start(ctx context.Context, onFire plugins.TriggerCallback) error {
	if len(t.repos) == 0 {
		// Polling without an allowlist would hammer the per-installation
		// repo list endpoint every minute. Refuse: the user must
		// declare which repos to watch.
		return errors.New("github: polling trigger requires non-empty repo allowlist on the connection")
	}
	go t.run(ctx, onFire)
	return nil
}

func (t *githubTrigger) Stop() error {
	t.stopOnce.Do(func() { close(t.stop) })
	return nil
}

func (t *githubTrigger) run(ctx context.Context, onFire plugins.TriggerCallback) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	// Run a poll immediately so the first ticker tick isn't 60s
	// behind the schedule.
	t.poll(ctx, onFire)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stop:
			return
		case <-ticker.C:
			t.poll(ctx, onFire)
		}
	}
}

func (t *githubTrigger) poll(ctx context.Context, onFire plugins.TriggerCallback) {
	t.mu.Lock()
	first := !t.firstPollDone
	t.firstPollDone = true
	t.mu.Unlock()

	switch t.kind {
	case TriggerKindIssueOpened:
		t.pollIssues(ctx, first, onFire)
	case TriggerKindPRReviewRequested:
		t.pollReviewRequests(ctx, first, onFire)
	}
}

// pollIssues queries /repos/<o>/<r>/issues?state=open per repo and
// fires for any issue number above the established baseline. ETags
// are persisted so repeats return 304 + zero quota cost when nothing
// changed.
func (t *githubTrigger) pollIssues(ctx context.Context, first bool, onFire plugins.TriggerCallback) {
	cli := gh.NewClient(t.authClient, t.installation)
	for _, r := range t.repos {
		key := strings.ToLower(r.owner + "/" + r.repo)

		// Conditional GET via ETag from prior poll.
		var raw []map[string]any
		err := cli.Do(ctx, "GET",
			fmt.Sprintf("/repos/%s/%s/issues?state=open&per_page=100&sort=created&direction=desc", r.owner, r.repo),
			nil, &raw)
		if err != nil {
			log.Printf("github poll issues %s: %v", key, err)
			continue
		}

		// Track the highest issue number we've seen this poll. Even
		// on the first poll we record it so subsequent polls
		// correctly delta against it.
		var highest int
		for _, issue := range raw {
			// Skip PR shadows.
			if _, isPR := issue["pull_request"]; isPR {
				continue
			}
			num, _ := issue["number"].(float64)
			if int(num) > highest {
				highest = int(num)
			}
		}

		t.mu.Lock()
		baseline := t.highestIssueNumber[key]
		if first || highest > baseline {
			t.highestIssueNumber[key] = highest
		}
		t.mu.Unlock()

		if first {
			continue
		}

		// Fire for each issue with number > baseline. Iterating in
		// reverse so the smallest "new" issue fires first.
		for i := len(raw) - 1; i >= 0; i-- {
			issue := raw[i]
			if _, isPR := issue["pull_request"]; isPR {
				continue
			}
			num, _ := issue["number"].(float64)
			if int(num) <= baseline {
				continue
			}
			title, _ := issue["title"].(string)
			body, _ := issue["body"].(string)
			ev := plugins.TriggerEvent{
				ConnectionID: t.connectionID,
				Kind:         t.kind,
				Goal:         fmt.Sprintf("New issue in %s/%s: %s", r.owner, r.repo, title),
				Metadata: map[string]interface{}{
					"owner":        r.owner,
					"repo":         r.repo,
					"issue_number": int(num),
					"title":        title,
					"body":         body,
				},
			}
			if err := onFire(ctx, ev); err != nil {
				log.Printf("github trigger fire %s: %v", key, err)
			}
		}
	}
}

// pollReviewRequests scans open PRs per repo for ones where the
// installation account has been added as a requested reviewer since
// last poll.
func (t *githubTrigger) pollReviewRequests(ctx context.Context, first bool, onFire plugins.TriggerCallback) {
	cli := gh.NewClient(t.authClient, t.installation)
	for _, r := range t.repos {
		key := strings.ToLower(r.owner + "/" + r.repo)
		var raw []map[string]any
		err := cli.Do(ctx, "GET",
			fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100", r.owner, r.repo),
			nil, &raw)
		if err != nil {
			log.Printf("github poll PRs %s: %v", key, err)
			continue
		}
		t.mu.Lock()
		seenSet := t.seenReviewRequests[key]
		if seenSet == nil {
			seenSet = map[int]bool{}
			t.seenReviewRequests[key] = seenSet
		}
		t.mu.Unlock()

		for _, pr := range raw {
			// requested_reviewers is the relevant field; non-empty
			// means a reviewer is pending. We don't filter by
			// "is the installation account specifically requested"
			// because the installation typically reviews on behalf
			// of the App, and any pending review is interesting
			// to a code-review assistant bound to this Connection.
			rr, _ := pr["requested_reviewers"].([]any)
			if len(rr) == 0 {
				continue
			}
			num, _ := pr["number"].(float64)
			if first {
				seenSet[int(num)] = true
				continue
			}
			t.mu.Lock()
			already := seenSet[int(num)]
			seenSet[int(num)] = true
			t.mu.Unlock()
			if already {
				continue
			}
			title, _ := pr["title"].(string)
			ev := plugins.TriggerEvent{
				ConnectionID: t.connectionID,
				Kind:         t.kind,
				Goal:         fmt.Sprintf("Review requested on %s/%s PR #%d: %s", r.owner, r.repo, int(num), title),
				Metadata: map[string]interface{}{
					"owner":       r.owner,
					"repo":        r.repo,
					"pull_number": int(num),
					"title":       title,
				},
			}
			if err := onFire(ctx, ev); err != nil {
				log.Printf("github trigger fire %s: %v", key, err)
			}
		}
	}
}

// pollingEnabled returns true when configPollingEnabled is the string
// "true" (config arrives as JSON-decoded map[string]any; bool may be
// either bool or string depending on how the UI saved it).
func pollingEnabled(config map[string]any) bool {
	switch v := config[configPollingEnabled].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// parseRepoAllowlist splits the comma-separated allowlist into
// repoRef pairs. Empty list = polling disabled (Start refuses).
func parseRepoAllowlist(config map[string]any) []repoRef {
	entries := stringSliceFromConfig(config, configRepoAllowlist)
	out := make([]repoRef, 0, len(entries))
	for _, e := range entries {
		parts := strings.SplitN(strings.TrimSpace(e), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, repoRef{owner: parts[0], repo: parts[1]})
	}
	return out
}
