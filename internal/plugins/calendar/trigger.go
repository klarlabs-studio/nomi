// Calendar trigger implementations (calendar-03). Three rule kinds:
//
//	calendar.pre_meeting    — fire N minutes before an event starts
//	calendar.event_created  — fire when a new event appears
//	calendar.event_cancelled — fire when an event disappears (or is cancelled)
//
// All three poll Provider.ListUpcoming on a configurable interval.
// Push notifications (Google Calendar's "watch" API) need the daemon
// to serve an externally-reachable HTTPS webhook, which is out of
// scope for v1's local-first install — polling is the universal
// fallback the spec calls out.
//
// Per-rule state lives in memory:
//   - seenEventIDs:    set of upcoming event ids observed last poll
//                      (drives event_created + event_cancelled deltas)
//   - firedPreMeeting: map of event id -> bool to prevent firing
//                      pre_meeting twice for the same event
//
// On daemon restart the seen set is empty, so the first poll
// reports every upcoming event as "created" — that's surprising
// noise, so we suppress firings on the very first poll the same
// way Gmail triggers do for their baseline.

package calendar

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

// Trigger kinds. The strings appear in connection config under
// trigger_rules[*].kind and in the manifest's TriggerContribution.Kind.
const (
	TriggerKindPreMeeting     = "calendar.pre_meeting"
	TriggerKindEventCreated   = "calendar.event_created"
	TriggerKindEventCancelled = "calendar.event_cancelled"
)

// defaultTriggerPoll matches Gmail's default. Calendar API rate
// limits are generous so the cadence is constrained more by user
// expectations of "freshness" than by quota.
const defaultTriggerPoll = 60 * time.Second

// defaultLookahead is the window ListUpcoming covers each poll.
// Events further out aren't visible to triggers — 24h covers
// pre_meeting leads up to a day in advance, which is the most any
// reasonable rule asks for.
const defaultLookahead = 24 * time.Hour

// TriggerRule mirrors Gmail's pattern. CalendarID is per-rule rather
// than per-connection so a user with multiple Google calendars can
// route different ones to different assistants. LeadMinutes is only
// meaningful for pre_meeting.
type TriggerRule struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	CalendarID  string `json:"calendar_id,omitempty"`
	LeadMinutes int    `json:"lead_minutes,omitempty"`
}

// rulesFromConfig parses the trigger_rules array. Same shape as the
// Gmail variant — kept separate (not a shared helper) so each plugin
// can evolve its rule schema independently.
func rulesFromConfig(cfg map[string]any) []TriggerRule {
	raw, ok := cfg["trigger_rules"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]TriggerRule, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		r := TriggerRule{
			Name:       asString(m, "name"),
			Kind:       asString(m, "kind"),
			CalendarID: asString(m, "calendar_id"),
		}
		switch v := m["lead_minutes"].(type) {
		case int:
			r.LeadMinutes = v
		case float64:
			r.LeadMinutes = int(v)
		}
		if r.Kind == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

func asString(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

// Triggers implements plugins.TriggerProvider. One Trigger per
// (connection × rule), mirroring Gmail's shape so the runtime can
// treat both plugins identically.
func (p *Plugin) Triggers() []plugins.Trigger {
	if p.connections == nil {
		return nil
	}
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		log.Printf("[calendar] list connections for triggers: %v", err)
		return nil
	}
	var out []plugins.Trigger
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		for _, rule := range rulesFromConfig(conn.Config) {
			t, err := p.newTrigger(conn, rule)
			if err != nil {
				log.Printf("[calendar] skipping rule %q on connection %s: %v", rule.Name, conn.ID, err)
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

func (p *Plugin) newTrigger(conn *domain.Connection, rule TriggerRule) (*calendarTrigger, error) {
	switch rule.Kind {
	case TriggerKindPreMeeting:
		if rule.LeadMinutes <= 0 {
			return nil, errors.New("pre_meeting requires lead_minutes > 0")
		}
	case TriggerKindEventCreated, TriggerKindEventCancelled:
		// no kind-specific config required
	default:
		return nil, fmt.Errorf("unknown trigger kind %q", rule.Kind)
	}
	return &calendarTrigger{
		plugin:          p,
		conn:            conn,
		rule:            rule,
		interval:        triggerPollInterval(conn),
		stopCh:          make(chan struct{}),
		firedPreMeeting: map[string]bool{},
	}, nil
}

func triggerPollInterval(conn *domain.Connection) time.Duration {
	if v, ok := conn.Config["poll_interval_seconds"].(float64); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultTriggerPoll
}

// calendarTrigger is one running Trigger instance.
type calendarTrigger struct {
	plugin   *Plugin
	conn     *domain.Connection
	rule     TriggerRule
	interval time.Duration

	mu              sync.Mutex
	seenEventIDs    map[string]Event // populated on first poll, kept fresh on every poll
	baselineDone    bool
	firedPreMeeting map[string]bool
	stopCh          chan struct{}
	stopOnce        sync.Once
	running         bool
	startedRunning  chan struct{}

	// nowFn is the clock the matcher consults; tests inject a fixed
	// time so pre-meeting math is deterministic.
	nowFn func() time.Time
}

func (t *calendarTrigger) ConnectionID() string { return t.conn.ID }
func (t *calendarTrigger) Kind() string         { return t.rule.Kind }

// now returns the trigger's clock. Defaults to time.Now; tests
// override via SetNow.
func (t *calendarTrigger) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

// SetNow swaps the clock for testing. Test-only seam.
func (t *calendarTrigger) SetNow(fn func() time.Time) { t.nowFn = fn }

// Start launches the poll loop.
func (t *calendarTrigger) Start(ctx context.Context, onFire plugins.TriggerCallback) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return nil
	}
	t.running = true
	t.startedRunning = make(chan struct{})
	t.mu.Unlock()
	go t.loop(ctx, onFire)
	return nil
}

// Stop ends the loop. Idempotent.
func (t *calendarTrigger) Stop() error {
	t.stopOnce.Do(func() { close(t.stopCh) })
	t.mu.Lock()
	t.running = false
	t.mu.Unlock()
	return nil
}

func (t *calendarTrigger) loop(ctx context.Context, onFire plugins.TriggerCallback) {
	close(t.startedRunning)
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	// Establish baseline immediately so a fast-firing pre_meeting
	// rule doesn't have to wait an extra interval.
	if err := t.pollOnce(ctx, onFire); err != nil {
		log.Printf("[calendar] trigger %q first-poll failed: %v", t.rule.Name, err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		case <-ticker.C:
			if err := t.pollOnce(ctx, onFire); err != nil {
				log.Printf("[calendar] trigger %q poll failed: %v", t.rule.Name, err)
			}
		}
	}
}

// pollOnce fetches the upcoming-events window, computes deltas
// against the last-seen set, and dispatches matching events. Pulled
// out of loop() so tests can drive single ticks deterministically.
func (t *calendarTrigger) pollOnce(ctx context.Context, onFire plugins.TriggerCallback) error {
	provider, err := t.plugin.providerFor(t.conn)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	now := t.now()
	events, err := provider.ListUpcoming(ctx, t.rule.CalendarID, now, now.Add(defaultLookahead), 0)
	if err != nil {
		return err
	}

	current := make(map[string]Event, len(events))
	for _, e := range events {
		current[e.ID] = e
	}

	t.mu.Lock()
	previous := t.seenEventIDs
	baseline := t.baselineDone
	t.seenEventIDs = current
	t.baselineDone = true
	// Drop firedPreMeeting entries for events that have already
	// passed — the map would grow unbounded otherwise.
	for id := range t.firedPreMeeting {
		if e, stillUpcoming := current[id]; !stillUpcoming || e.Start.Before(now) {
			delete(t.firedPreMeeting, id)
		}
	}
	t.mu.Unlock()

	if !baseline {
		// First poll establishes the seen set; suppress event_created
		// firings so existing calendar events don't all retro-fire at
		// daemon start. pre_meeting still fires on the baseline poll
		// because a meeting starting in 5 minutes shouldn't wait an
		// extra interval before the user is notified.
		if t.rule.Kind == TriggerKindPreMeeting {
			t.fireMatchingPreMeeting(ctx, onFire, current, now)
		}
		return nil
	}

	switch t.rule.Kind {
	case TriggerKindEventCreated:
		for id, e := range current {
			if _, existed := previous[id]; existed {
				continue
			}
			if err := onFire(ctx, t.toTriggerEvent(e)); err != nil {
				log.Printf("[calendar] trigger %q onFire returned: %v", t.rule.Name, err)
			}
		}
	case TriggerKindEventCancelled:
		for id, e := range previous {
			if _, stillThere := current[id]; stillThere {
				continue
			}
			// Don't fire cancellation for events that simply rolled
			// out of the lookahead window because their start time
			// passed.
			if e.Start.Before(now) {
				continue
			}
			if err := onFire(ctx, t.toTriggerEvent(e)); err != nil {
				log.Printf("[calendar] trigger %q onFire returned: %v", t.rule.Name, err)
			}
		}
	case TriggerKindPreMeeting:
		t.fireMatchingPreMeeting(ctx, onFire, current, now)
	}
	return nil
}

// fireMatchingPreMeeting checks each upcoming event against the
// rule's lead window. An event whose Start falls within
// [now+lead-interval, now+lead] qualifies — the interval-wide
// window catches the boundary case where a poll happens slightly
// later than expected. Each event fires at most once per
// trigger lifetime via firedPreMeeting.
func (t *calendarTrigger) fireMatchingPreMeeting(ctx context.Context, onFire plugins.TriggerCallback, events map[string]Event, now time.Time) {
	lead := time.Duration(t.rule.LeadMinutes) * time.Minute
	windowStart := now.Add(lead - t.interval)
	windowEnd := now.Add(lead)
	for id, e := range events {
		t.mu.Lock()
		alreadyFired := t.firedPreMeeting[id]
		t.mu.Unlock()
		if alreadyFired {
			continue
		}
		if e.Start.Before(windowStart) || e.Start.After(windowEnd) {
			continue
		}
		t.mu.Lock()
		t.firedPreMeeting[id] = true
		t.mu.Unlock()
		if err := onFire(ctx, t.toTriggerEvent(e)); err != nil {
			log.Printf("[calendar] trigger %q onFire returned: %v", t.rule.Name, err)
		}
	}
}

func (t *calendarTrigger) toTriggerEvent(e Event) plugins.TriggerEvent {
	var goal string
	switch t.rule.Kind {
	case TriggerKindPreMeeting:
		goal = fmt.Sprintf("Calendar rule %q: meeting %q starts in %d min", t.rule.Name, e.Title, t.rule.LeadMinutes)
	case TriggerKindEventCreated:
		goal = fmt.Sprintf("Calendar rule %q: new event %q at %s", t.rule.Name, e.Title, e.Start.Format(time.RFC3339))
	case TriggerKindEventCancelled:
		goal = fmt.Sprintf("Calendar rule %q: event cancelled — %q (was at %s)", t.rule.Name, e.Title, e.Start.Format(time.RFC3339))
	}
	return plugins.TriggerEvent{
		ConnectionID: t.conn.ID,
		Kind:         t.rule.Kind,
		Goal:         goal,
		Metadata: map[string]interface{}{
			"rule_name":   t.rule.Name,
			"event_id":    e.ID,
			"event_title": e.Title,
			"event_start": e.Start,
			"event_end":   e.End,
			"location":    e.Location,
			"attendees":   e.Attendees,
		},
	}
}
