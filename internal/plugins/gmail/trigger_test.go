package gmail

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// triggerFixture builds a plugin + connection with the given
// trigger_rules array baked into the connection config, then exposes
// helpers to drive the per-trigger pollOnce path deterministically.
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
	t.Cleanup(func() { database.Close() })
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
	connID := "conn-trigger-1"
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

// --- Manifest declares all three trigger kinds ---------------------

func TestManifest_DeclaresAllTriggerKinds(t *testing.T) {
	m := NewPlugin(nil, nil, nil, nil).Manifest()
	want := map[string]bool{
		TriggerKindFromWatch:  false,
		TriggerKindLabelWatch: false,
		TriggerKindQueryWatch: false,
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

// --- Triggers() materializes one per (connection, rule) -----------

func TestTriggers_OnePerRule(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Boss", "kind": TriggerKindFromWatch, "from": "boss@example.com"},
		{"name": "Star", "kind": TriggerKindLabelWatch, "label": "STARRED"},
	})
	got := f.plugin.Triggers()
	if len(got) != 2 {
		t.Fatalf("expected 2 triggers, got %d", len(got))
	}
}

func TestTriggers_SkipsMalformedRules(t *testing.T) {
	// Missing required field per kind → rule silently dropped, others
	// still materialize. Logs the skip but doesn't fail the plugin.
	f := newTriggerFixture(t, []map[string]any{
		{"name": "MissingFrom", "kind": TriggerKindFromWatch /* no from */},
		{"name": "GoodLabel", "kind": TriggerKindLabelWatch, "label": "STARRED"},
		{"name": "UnknownKind", "kind": "gmail.future_thing"},
	})
	got := f.plugin.Triggers()
	if len(got) != 1 {
		t.Fatalf("expected 1 surviving trigger, got %d", len(got))
	}
	if got[0].Kind() != TriggerKindLabelWatch {
		t.Fatalf("survivor = %q, want label_watch", got[0].Kind())
	}
}

func TestTriggers_SkipsDisabledConnections(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "X", "kind": TriggerKindFromWatch, "from": "boss@example.com"},
	})
	c, _ := f.connsRepo.GetByID(f.connID)
	c.Enabled = false
	if err := f.connsRepo.Update(c); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := f.plugin.Triggers(); len(got) != 0 {
		t.Fatalf("expected 0 triggers on disabled connection, got %d", len(got))
	}
}

// --- pollOnce semantics --------------------------------------------

// pollOnceHelper exposes the unexported pollOnce + establishBaseline
// methods so the deterministic tests don't have to wait for a tick.
func driveBaselineAndPoll(t *testing.T, trig *gmailTrigger, onFire plugins.TriggerCallback) {
	t.Helper()
	if err := trig.establishBaseline(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if err := trig.pollOnce(context.Background(), onFire); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
}

func TestPollOnce_FromWatch_FiresOnMatchingSender(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Boss", "kind": TriggerKindFromWatch, "from": "boss@example.com"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)

	f.provider.latestHistoryID = "100"
	f.provider.historyNewestID = "200"
	f.provider.historyEvents = []HistoryEvent{
		{HistoryID: "150", Kind: HistoryMessageAdded, MessageID: "m-from-boss", ThreadID: "t1"},
		{HistoryID: "160", Kind: HistoryMessageAdded, MessageID: "m-from-other", ThreadID: "t2"},
	}
	f.provider.getMessageOut = map[string]Message{
		"m-from-boss":  {ID: "m-from-boss", From: "Boss <boss@example.com>", Subject: "Q3", ThreadID: "t1"},
		"m-from-other": {ID: "m-from-other", From: "spam@x.invalid", Subject: "buy", ThreadID: "t2"},
	}

	var fired []plugins.TriggerEvent
	var mu sync.Mutex
	onFire := func(_ context.Context, ev plugins.TriggerEvent) error {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
		return nil
	}
	driveBaselineAndPoll(t, trig, onFire)

	if len(fired) != 1 {
		t.Fatalf("fired = %d, want 1: %+v", len(fired), fired)
	}
	if fired[0].Metadata["message_id"] != "m-from-boss" {
		t.Fatalf("wrong message fired: %+v", fired[0].Metadata)
	}
	if fired[0].Kind != TriggerKindFromWatch {
		t.Fatalf("kind = %q, want %q", fired[0].Kind, TriggerKindFromWatch)
	}
	if fired[0].ConnectionID != f.connID {
		t.Fatalf("connection_id = %q", fired[0].ConnectionID)
	}
}

func TestPollOnce_LabelWatch_FiresOnMatchingLabel(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Star", "kind": TriggerKindLabelWatch, "label": "STARRED"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)

	f.provider.latestHistoryID = "100"
	f.provider.historyNewestID = "200"
	f.provider.historyEvents = []HistoryEvent{
		{HistoryID: "150", Kind: HistoryLabelAdded, MessageID: "m1", ThreadID: "t1", AddedLabelIDs: []string{"STARRED"}},
		{HistoryID: "160", Kind: HistoryLabelAdded, MessageID: "m2", ThreadID: "t2", AddedLabelIDs: []string{"INBOX"}},
	}
	f.provider.getMessageOut = map[string]Message{
		"m1": {ID: "m1", Subject: "first", ThreadID: "t1"},
	}

	var fired []plugins.TriggerEvent
	var mu sync.Mutex
	onFire := func(_ context.Context, ev plugins.TriggerEvent) error {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, ev)
		return nil
	}
	driveBaselineAndPoll(t, trig, onFire)

	if len(fired) != 1 {
		t.Fatalf("fired = %d, want 1: %+v", len(fired), fired)
	}
	if fired[0].Metadata["message_id"] != "m1" {
		t.Fatalf("wrong message fired: %+v", fired[0].Metadata)
	}
}

func TestPollOnce_QueryWatch_FiresWhenSearchFindsMessage(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Alerts", "kind": TriggerKindQueryWatch, "query": "from:alerts subject:critical"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)

	f.provider.latestHistoryID = "100"
	f.provider.historyNewestID = "200"
	f.provider.historyEvents = []HistoryEvent{
		{HistoryID: "150", Kind: HistoryMessageAdded, MessageID: "m-alert", ThreadID: "ta"},
	}
	f.provider.getMessageOut = map[string]Message{
		"m-alert": {ID: "m-alert", From: "alerts@x.io", Subject: "critical: db down", ThreadID: "ta"},
	}
	// SearchThreads returning a hit means the query matched.
	f.provider.searchOut = []Thread{{ID: "ta"}}

	var fired []plugins.TriggerEvent
	onFire := func(_ context.Context, ev plugins.TriggerEvent) error {
		fired = append(fired, ev)
		return nil
	}
	driveBaselineAndPoll(t, trig, onFire)

	if len(fired) != 1 {
		t.Fatalf("fired = %d, want 1: %+v", len(fired), fired)
	}
	// The provider stub should have seen the rfc822msgid suffix appended.
	if !contains(f.provider.searched, "rfc822msgid:m-alert") {
		t.Fatalf("expected query to be narrowed by message id, got %q", f.provider.searched)
	}
}

func TestPollOnce_Baseline_NoFireOnFirstTick(t *testing.T) {
	// The very first tick after Start establishes the baseline
	// historyId via LatestHistoryID — it should NOT fetch History
	// (and so MUST NOT fire) on that first tick. Establishing a
	// baseline uses LatestHistoryID; the next pollOnce uses History.
	f := newTriggerFixture(t, []map[string]any{
		{"name": "Boss", "kind": TriggerKindFromWatch, "from": "boss@example.com"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)

	f.provider.latestHistoryID = "100"
	// Even if history would return events, the first tick (baseline)
	// shouldn't ask for them.
	if err := trig.establishBaseline(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(f.provider.historyStartIDs) != 0 {
		t.Fatalf("baseline should not call History, got start_ids=%v", f.provider.historyStartIDs)
	}
	if trig.lastHistoryID != "100" {
		t.Fatalf("lastHistoryID = %q, want 100", trig.lastHistoryID)
	}
}

func TestPollOnce_AdvancesCursor(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "X", "kind": TriggerKindFromWatch, "from": "x@x.x"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)
	f.provider.latestHistoryID = "100"
	if err := trig.establishBaseline(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// First poll: History returns newest=200; cursor should advance.
	f.provider.historyNewestID = "200"
	if err := trig.pollOnce(context.Background(), noopFire); err != nil {
		t.Fatalf("pollOnce 1: %v", err)
	}
	if trig.lastHistoryID != "200" {
		t.Fatalf("after 1st poll lastHistoryID = %q, want 200", trig.lastHistoryID)
	}
	// Second poll uses 200 as the start.
	f.provider.historyNewestID = "300"
	if err := trig.pollOnce(context.Background(), noopFire); err != nil {
		t.Fatalf("pollOnce 2: %v", err)
	}
	if trig.lastHistoryID != "300" {
		t.Fatalf("after 2nd poll lastHistoryID = %q, want 300", trig.lastHistoryID)
	}
	if got := f.provider.historyStartIDs; len(got) != 2 || got[0] != "100" || got[1] != "200" {
		t.Fatalf("history start_ids drift: %v", got)
	}
}

func TestPollOnce_HistoryErrorIsTransient(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "X", "kind": TriggerKindFromWatch, "from": "x@x.x"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)
	f.provider.latestHistoryID = "100"
	_ = trig.establishBaseline(context.Background())

	f.provider.historyErr = errors.New("transient")
	err := trig.pollOnce(context.Background(), noopFire)
	if err == nil {
		t.Fatal("expected pollOnce to surface history error")
	}
	// Cursor must NOT advance on error — the next poll should retry
	// the same start_id.
	if trig.lastHistoryID != "100" {
		t.Fatalf("lastHistoryID drifted on error: %q", trig.lastHistoryID)
	}
}

func TestStartStop_Idempotent(t *testing.T) {
	f := newTriggerFixture(t, []map[string]any{
		{"name": "X", "kind": TriggerKindFromWatch, "from": "x@x.x"},
	})
	trig := f.plugin.Triggers()[0].(*gmailTrigger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := trig.Start(ctx, noopFire); err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	if err := trig.Start(ctx, noopFire); err != nil {
		t.Fatalf("Start 2 should be no-op, got %v", err)
	}
	// Wait a beat to make sure the goroutine is up before Stop, so
	// Stop's close(stopCh) actually unblocks the loop.
	<-trig.startedRunning
	if err := trig.Stop(); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}
	if err := trig.Stop(); err != nil {
		t.Fatalf("Stop 2 should be idempotent, got %v", err)
	}
}

func TestTriggerPollInterval_Override(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{"poll_interval_seconds": float64(5)}}
	if got := triggerPollInterval(conn); got != 5*time.Second {
		t.Fatalf("interval override = %s, want 5s", got)
	}
	def := &domain.Connection{Config: map[string]any{}}
	if got := triggerPollInterval(def); got != defaultTriggerPoll {
		t.Fatalf("default interval = %s", got)
	}
}

func TestRulesFromConfig_HandlesEmptyAndMalformed(t *testing.T) {
	if got := rulesFromConfig(nil); got != nil {
		t.Fatalf("nil config should yield nil rules, got %v", got)
	}
	if got := rulesFromConfig(map[string]any{"trigger_rules": "wrong-type"}); got != nil {
		t.Fatalf("wrong-type field should yield nil rules, got %v", got)
	}
	got := rulesFromConfig(map[string]any{
		"trigger_rules": []interface{}{
			map[string]interface{}{"kind": "x", "name": "n"},
			"not-an-object",
		},
	})
	if len(got) != 1 || got[0].Name != "n" {
		t.Fatalf("expected the one good rule, got %v", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		(len(haystack) > len(needle) && (haystack[:len(needle)] == needle ||
			haystack[len(haystack)-len(needle):] == needle ||
			containsSlow(haystack, needle))))
}

// Avoiding strings.Contains import noise — this is the only call.
func containsSlow(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func noopFire(_ context.Context, _ plugins.TriggerEvent) error { return nil }
