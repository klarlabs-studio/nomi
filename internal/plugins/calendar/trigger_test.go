package calendar

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// stubProvider serves canned upcoming-event lists per call so the
// trigger pollOnce path is exercisable without a real Calendar
// account.
type stubProvider struct {
	mu       sync.Mutex
	upcoming []Event
	calls    int
}

func (s *stubProvider) ListUpcoming(_ context.Context, _ string, _, _ time.Time, _ int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	out := make([]Event, len(s.upcoming))
	copy(out, s.upcoming)
	return out, nil
}

// The other Provider methods aren't exercised by triggers, but the
// interface needs satisfying.
func (s *stubProvider) CreateEvent(context.Context, string, Event) (Event, error) {
	return Event{}, nil
}
func (s *stubProvider) UpdateEvent(context.Context, string, string, Event) (Event, error) {
	return Event{}, nil
}
func (s *stubProvider) DeleteEvent(context.Context, string, string) error { return nil }
func (s *stubProvider) FindFreeSlots(context.Context, []string, time.Time, time.Time, time.Duration) ([]FreeSlot, error) {
	return nil, nil
}

// triggerFixture wires a plugin + connection + stub provider. The
// connection's trigger_rules array drives Triggers().
type triggerFixture struct {
	plugin    *Plugin
	connID    string
	provider  *stubProvider
	connsRepo *db.ConnectionRepository
}

func newTriggerFixture(t *testing.T, rules []map[string]any) *triggerFixture {
	t.Helper()
	tmp := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(tmp, "test.db")})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conns := db.NewConnectionRepository(database)
	binds := db.NewAssistantBindingRepository(database)
	plug := NewPlugin(conns, binds, nil, nil)
	provider := &stubProvider{}
	plug.SetProviderOverride(func(*domain.Connection) (Provider, error) { return provider, nil })

	rawRules := make([]interface{}, 0, len(rules))
	for _, r := range rules {
		rawRules = append(rawRules, r)
	}
	connID := "conn-cal-trigger-1"
	if err := conns.Create(&domain.Connection{
		ID:       connID,
		PluginID: PluginID,
		Name:     "test",
		Config: map[string]any{
			"provider":      "google",
			"account_id":    "acct",
			"client_id":     "cid",
			"trigger_rules": rawRules,
		},
		Enabled: true,
	}); err != nil {
		t.Fatalf("conn create: %v", err)
	}
	return &triggerFixture{plugin: plug, connID: connID, provider: provider, connsRepo: conns}
}

// --- manifest declares all three trigger kinds ---------------------

func TestManifest_DeclaresAllTriggerKinds(t *testing.T) {
	m := NewPlugin(nil, nil, nil, nil).Manifest()
	want := map[string]bool{
		TriggerKindPreMeeting:     false,
		TriggerKindEventCreated:   false,
		TriggerKindEventCancelled: false,
	}
	for _, tc := range m.Contributes.Triggers {
		want[tc.Kind] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("trigger kind %q missing from manifest", k)
		}
	}
}

// --- Triggers() materialization ------------------------------------

func TestTriggers_OnePerRule(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Standup", "kind": TriggerKindPreMeeting, "lead_minutes": 5},
		{"name": "New", "kind": TriggerKindEventCreated},
	})
	if got := f.plugin.Triggers(); len(got) != 2 {
		t.Fatalf("want 2 triggers, got %d", len(got))
	}
}

func TestTriggers_RejectsBadRules(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "PreNoLead", "kind": TriggerKindPreMeeting}, // missing lead_minutes
		{"name": "Unknown", "kind": "calendar.future_thing"}, // unknown kind
		{"name": "OK", "kind": TriggerKindEventCancelled},    // valid
	})
	got := f.plugin.Triggers()
	if len(got) != 1 || got[0].Kind() != TriggerKindEventCancelled {
		t.Fatalf("survivor mismatch, got %v", got)
	}
}

// --- pollOnce semantics -------------------------------------------

// captureFire returns an onFire callback that appends events to a
// slice the test can later inspect.
func captureFire() (*[]plugins.TriggerEvent, plugins.TriggerCallback) {
	var fired []plugins.TriggerEvent
	var mu sync.Mutex
	cb := func(_ context.Context, ev plugins.TriggerEvent) error {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
		return nil
	}
	return &fired, cb
}

func TestEventCreated_BaselineSuppressesFirstPoll(t *testing.T) {
	// First poll must NOT fire event_created on existing events —
	// otherwise daemon restart retro-fires the entire next-24h calendar.
	f := newTriggerFixture(t, []map[string]any{
		{"name": "New", "kind": TriggerKindEventCreated},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })
	f.provider.upcoming = []Event{
		{ID: "e1", Title: "existing", Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
	}

	fired, cb := captureFire()
	if err := trig.pollOnce(context.Background(), cb); err != nil {
		t.Fatalf("pollOnce 1: %v", err)
	}
	if len(*fired) != 0 {
		t.Fatalf("baseline poll should not fire, got %v", *fired)
	}
}

func TestEventCreated_FiresOnNewEvent(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "New", "kind": TriggerKindEventCreated},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })

	// Baseline: existing event already there, no fire.
	f.provider.upcoming = []Event{{ID: "e1", Title: "existing", Start: now.Add(2 * time.Hour)}}
	_, cb := captureFire()
	_ = trig.pollOnce(context.Background(), cb)

	// Second poll: a new event appears → fires.
	f.provider.upcoming = []Event{
		{ID: "e1", Title: "existing", Start: now.Add(2 * time.Hour)},
		{ID: "e2", Title: "new", Start: now.Add(4 * time.Hour)},
	}
	fired, cb := captureFire()
	if err := trig.pollOnce(context.Background(), cb); err != nil {
		t.Fatalf("pollOnce 2: %v", err)
	}
	if len(*fired) != 1 || (*fired)[0].Metadata["event_id"] != "e2" {
		t.Fatalf("expected fire for e2, got %v", *fired)
	}
}

func TestEventCancelled_FiresWhenEventDisappears(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Cancel", "kind": TriggerKindEventCancelled},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })

	// Baseline: two events.
	f.provider.upcoming = []Event{
		{ID: "e1", Title: "first", Start: now.Add(2 * time.Hour)},
		{ID: "e2", Title: "second", Start: now.Add(4 * time.Hour)},
	}
	_, cb := captureFire()
	_ = trig.pollOnce(context.Background(), cb)

	// Second poll: e2 vanishes → fires cancellation.
	f.provider.upcoming = []Event{
		{ID: "e1", Title: "first", Start: now.Add(2 * time.Hour)},
	}
	fired, cb := captureFire()
	if err := trig.pollOnce(context.Background(), cb); err != nil {
		t.Fatalf("pollOnce 2: %v", err)
	}
	if len(*fired) != 1 || (*fired)[0].Metadata["event_id"] != "e2" {
		t.Fatalf("expected cancellation for e2, got %v", *fired)
	}
}

func TestEventCancelled_DoesNotFireForElapsedEvents(t *testing.T) {
	// Events that simply rolled past their start time and dropped
	// out of the lookahead window must NOT fire as cancellations.
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Cancel", "kind": TriggerKindEventCancelled},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })

	// Baseline includes an event starting in 1 hour.
	f.provider.upcoming = []Event{
		{ID: "e-elapsed", Title: "soon", Start: now.Add(1 * time.Hour)},
	}
	_, cb := captureFire()
	_ = trig.pollOnce(context.Background(), cb)

	// Advance the clock past the event's start time and stop
	// returning it from ListUpcoming.
	trig.SetNow(func() time.Time { return now.Add(2 * time.Hour) })
	f.provider.upcoming = nil
	fired, cb := captureFire()
	if err := trig.pollOnce(context.Background(), cb); err != nil {
		t.Fatalf("pollOnce 2: %v", err)
	}
	if len(*fired) != 0 {
		t.Fatalf("elapsed event should not fire cancellation, got %v", *fired)
	}
}

func TestPreMeeting_FiresWithinLeadWindow(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "5min", "kind": TriggerKindPreMeeting, "lead_minutes": 5},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })

	// Event starting in 4 minutes — inside the [now+lead-interval, now+lead] window.
	f.provider.upcoming = []Event{
		{ID: "e-soon", Title: "standup", Start: now.Add(4 * time.Minute)},
	}
	fired, cb := captureFire()
	if err := trig.pollOnce(context.Background(), cb); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(*fired) != 1 {
		t.Fatalf("expected pre-meeting fire, got %v", *fired)
	}
	if (*fired)[0].Kind != TriggerKindPreMeeting {
		t.Fatalf("kind drift: %q", (*fired)[0].Kind)
	}
}

func TestPreMeeting_DoesNotFireForFarFutureEvent(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "5min", "kind": TriggerKindPreMeeting, "lead_minutes": 5},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })

	f.provider.upcoming = []Event{
		{ID: "e-far", Title: "next week", Start: now.Add(2 * time.Hour)},
	}
	fired, cb := captureFire()
	_ = trig.pollOnce(context.Background(), cb)
	if len(*fired) != 0 {
		t.Fatalf("far-future event should not fire pre_meeting, got %v", *fired)
	}
}

func TestPreMeeting_FiresOnlyOnce(t *testing.T) {
	// pre_meeting must not re-fire across multiple polls within the
	// same window — firedPreMeeting tracks this. Without dedupe a
	// 5-minute lead with a 60s poll would fire 5 times.
	f := newTriggerFixture(t, []map[string]any{
		{"name": "5min", "kind": TriggerKindPreMeeting, "lead_minutes": 5},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })

	f.provider.upcoming = []Event{
		{ID: "e1", Title: "standup", Start: now.Add(4 * time.Minute)},
	}
	_, cb := captureFire()
	_ = trig.pollOnce(context.Background(), cb)

	// A second poll while the window is still active should not refire.
	fired, cb := captureFire()
	_ = trig.pollOnce(context.Background(), cb)
	if len(*fired) != 0 {
		t.Fatalf("pre_meeting refired within window: %v", *fired)
	}
}

func TestPreMeeting_RuleRequiresLeadMinutes(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Bad", "kind": TriggerKindPreMeeting},
	})
	if got := f.plugin.Triggers(); len(got) != 0 {
		t.Fatalf("expected zero triggers for invalid pre_meeting rule, got %d", len(got))
	}
}

// --- StartStop -----------------------------------------------------

func TestStartStop_Idempotent(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "X", "kind": TriggerKindEventCreated},
	})
	trig := f.plugin.Triggers()[0].(*calendarTrigger)
	now := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	trig.SetNow(func() time.Time { return now })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := trig.Start(ctx, func(_ context.Context, _ plugins.TriggerEvent) error { return nil }); err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	if err := trig.Start(ctx, func(_ context.Context, _ plugins.TriggerEvent) error { return nil }); err != nil {
		t.Fatalf("Start 2 should be no-op: %v", err)
	}
	<-trig.startedRunning
	if err := trig.Stop(); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}
	if err := trig.Stop(); err != nil {
		t.Fatalf("Stop 2: %v", err)
	}
}

// --- rule parsing -------------------------------------------------

func TestRulesFromConfig_HandlesEmptyAndMalformed(t *testing.T) {
	if got := rulesFromConfig(nil); got != nil {
		t.Fatalf("nil cfg should yield nil")
	}
	if got := rulesFromConfig(map[string]any{"trigger_rules": "wrong-type"}); got != nil {
		t.Fatalf("wrong type should yield nil")
	}
	got := rulesFromConfig(map[string]any{
		"trigger_rules": []interface{}{
			map[string]interface{}{"kind": "x", "name": "n", "lead_minutes": float64(7)},
			"not-an-object",
		},
	})
	if len(got) != 1 || got[0].LeadMinutes != 7 {
		t.Fatalf("rule parse drift: %+v", got)
	}
}

func TestTriggerPollInterval_Override(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{"poll_interval_seconds": float64(10)}}
	if got := triggerPollInterval(conn); got != 10*time.Second {
		t.Fatalf("interval override = %s, want 10s", got)
	}
	def := &domain.Connection{Config: map[string]any{}}
	if got := triggerPollInterval(def); got != defaultTriggerPoll {
		t.Fatalf("default interval = %s", got)
	}
}
