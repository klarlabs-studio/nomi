// Gmail trigger implementations (gmail-03). Three rule kinds:
//
//	from_watch    — fire when a new message arrives From a configured sender
//	label_watch   — fire when an existing message gains a configured label
//	query_watch   — fire when a new message matches a Gmail-search query
//
// All three use the same poll loop driven by Gmail's history API. The
// API returns just the deltas since startHistoryId (and we restrict
// historyTypes to messageAdded + labelAdded), so a high-volume
// mailbox doesn't pay full-mailbox-scan cost on every tick.
//
// Per-rule trigger state (last historyId, baseline established) lives
// in memory on the trigger struct. After a daemon restart the
// trigger re-establishes a fresh baseline, which means events that
// landed during the downtime are not retro-fired. That's the right
// default for v1 — re-firing arbitrarily-old mail at restart is far
// more surprising than missing it.

package gmail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

// Trigger kinds. The strings are the wire format users see in their
// connection config under trigger_rules[*].kind.
const (
	TriggerKindFromWatch  = "gmail.from_watch"
	TriggerKindLabelWatch = "gmail.label_watch"
	TriggerKindQueryWatch = "gmail.query_watch"
)

// defaultTriggerPoll is the per-rule poll cadence when the connection
// config doesn't override it. 60s is the same default as the email
// plugin's IMAP poll and a comfortable middle ground between
// freshness and Gmail rate-limit comfort.
const defaultTriggerPoll = 60 * time.Second

// TriggerRule is one row from the connection's trigger_rules array.
// Name surfaces in the run goal text + audit log so users can spot
// which rule fired. Fields are kind-specific:
//   - from_watch  → From
//   - label_watch → Label  (Gmail label id, e.g. "Label_123" or "STARRED")
//   - query_watch → Query  (Gmail search syntax)
type TriggerRule struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	From  string `json:"from,omitempty"`
	Label string `json:"label,omitempty"`
	Query string `json:"query,omitempty"`
}

// rulesFromConfig parses the trigger_rules array out of a connection's
// JSON-shaped Config. Returns an empty slice when the field is
// absent, malformed, or empty — a misconfigured rule should not
// crash the plugin, just silently skip.
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
			Name:  asString(m, "name"),
			Kind:  asString(m, "kind"),
			From:  asString(m, "from"),
			Label: asString(m, "label"),
			Query: asString(m, "query"),
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

// Triggers implements plugins.TriggerProvider. One trigger per
// (connection, rule) pair so each rule polls independently — keeps
// the state model simple and isolates failure (a hung query for one
// rule doesn't block others).
func (p *Plugin) Triggers() []plugins.Trigger {
	if p.connections == nil {
		return nil
	}
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		log.Printf("[gmail] list connections for triggers: %v", err)
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
				log.Printf("[gmail] skipping rule %q on connection %s: %v", rule.Name, conn.ID, err)
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

// newTrigger validates a rule and constructs the runtime trigger. The
// kind string drives both the manifest TriggerContribution.Kind and
// the dispatch logic in the poll loop.
func (p *Plugin) newTrigger(conn *domain.Connection, rule TriggerRule) (*gmailTrigger, error) {
	switch rule.Kind {
	case TriggerKindFromWatch:
		if rule.From == "" {
			return nil, errors.New("from_watch requires `from`")
		}
	case TriggerKindLabelWatch:
		if rule.Label == "" {
			return nil, errors.New("label_watch requires `label`")
		}
	case TriggerKindQueryWatch:
		if rule.Query == "" {
			return nil, errors.New("query_watch requires `query`")
		}
	default:
		return nil, fmt.Errorf("unknown trigger kind %q", rule.Kind)
	}
	return &gmailTrigger{
		plugin:   p,
		conn:     conn,
		rule:     rule,
		interval: triggerPollInterval(conn),
		stopCh:   make(chan struct{}),
	}, nil
}

// triggerPollInterval reads poll_interval_seconds from the connection
// config, defaulting to defaultTriggerPoll. The setting is shared
// across all triggers on a connection so the Gmail rate-limit budget
// is easy to reason about.
func triggerPollInterval(conn *domain.Connection) time.Duration {
	if v, ok := conn.Config["poll_interval_seconds"].(float64); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultTriggerPoll
}

// gmailTrigger is one running Trigger instance — one per (connection,
// rule). Holds its own goroutine + stop channel; the runtime calls
// Start/Stop and the trigger fires events through the supplied
// callback.
type gmailTrigger struct {
	plugin   *Plugin
	conn     *domain.Connection
	rule     TriggerRule
	interval time.Duration

	mu             sync.Mutex
	lastHistoryID  string
	stopCh         chan struct{}
	stopOnce       sync.Once
	running        bool
	startedRunning chan struct{} // closed once the goroutine has booted
}

// ConnectionID implements plugins.Trigger.
func (t *gmailTrigger) ConnectionID() string { return t.conn.ID }

// Kind implements plugins.Trigger — returns the rule kind so the
// runtime can map (connection, kind) back to assistant bindings.
func (t *gmailTrigger) Kind() string { return t.rule.Kind }

// Start begins the poll loop. Returns once the loop is launched (not
// once it's seen its first event). onFire is called from the
// goroutine; the runtime is expected to be safe for concurrent
// invocations.
func (t *gmailTrigger) Start(ctx context.Context, onFire plugins.TriggerCallback) error {
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

// Stop ends the poll loop. Idempotent; safe to call before Start.
func (t *gmailTrigger) Stop() error {
	t.stopOnce.Do(func() { close(t.stopCh) })
	t.mu.Lock()
	t.running = false
	t.mu.Unlock()
	return nil
}

// loop is the per-trigger goroutine. First tick establishes a
// historyId baseline (no events fire); subsequent ticks ask Gmail
// for deltas via the History API and dispatch each matching event.
func (t *gmailTrigger) loop(ctx context.Context, onFire plugins.TriggerCallback) {
	close(t.startedRunning)
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()

	if err := t.establishBaseline(ctx); err != nil {
		log.Printf("[gmail] trigger %q baseline failed: %v", t.rule.Name, err)
		// Keep looping — transient failures should be retried, not
		// silently exit. The next tick will retry establishBaseline.
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		case <-ticker.C:
			if err := t.pollOnce(ctx, onFire); err != nil {
				log.Printf("[gmail] trigger %q poll failed: %v", t.rule.Name, err)
			}
		}
	}
}

// establishBaseline calls LatestHistoryID and stores it as the
// startHistoryID for the next History call. Without this, the first
// History call would either fail (Gmail wants a starting point) or
// fire on every existing message, neither of which is desirable.
func (t *gmailTrigger) establishBaseline(ctx context.Context) error {
	t.mu.Lock()
	if t.lastHistoryID != "" {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	provider, err := t.plugin.providerFor(t.conn)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	id, err := provider.LatestHistoryID(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.lastHistoryID = id
	t.mu.Unlock()
	return nil
}

// pollOnce fetches deltas since lastHistoryID, advances the cursor,
// and fires onFire for every matching event. Pulled out of loop()
// so unit tests can drive a single tick deterministically.
func (t *gmailTrigger) pollOnce(ctx context.Context, onFire plugins.TriggerCallback) error {
	t.mu.Lock()
	startID := t.lastHistoryID
	t.mu.Unlock()
	if startID == "" {
		// Baseline failed earlier — try again this tick.
		return t.establishBaseline(ctx)
	}

	provider, err := t.plugin.providerFor(t.conn)
	if err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	labelFilter := ""
	if t.rule.Kind == TriggerKindLabelWatch {
		labelFilter = t.rule.Label
	}
	events, newest, err := provider.History(ctx, startID, labelFilter)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.lastHistoryID = newest
	t.mu.Unlock()

	for _, ev := range events {
		matched, msg, err := t.matches(ctx, provider, ev)
		if err != nil {
			log.Printf("[gmail] trigger %q match failed for message %s: %v", t.rule.Name, ev.MessageID, err)
			continue
		}
		if !matched {
			continue
		}
		if err := onFire(ctx, t.toTriggerEvent(msg)); err != nil {
			// Non-fatal — runtime decides whether the event maps to a
			// run. Log and continue with the next event.
			log.Printf("[gmail] trigger %q onFire returned: %v", t.rule.Name, err)
		}
	}
	return nil
}

// matches decides whether a HistoryEvent satisfies the rule. For
// from_watch and query_watch we fetch the message metadata to read
// the From header (or run a lightweight header check); for
// label_watch the AddedLabelIDs slice is enough.
func (t *gmailTrigger) matches(ctx context.Context, provider Provider, ev HistoryEvent) (bool, Message, error) {
	switch t.rule.Kind {
	case TriggerKindLabelWatch:
		// Label events arrive with the labels that were just added;
		// match when our configured label is among them. The server
		// labelFilter narrowed already but we re-check defensively.
		for _, l := range ev.AddedLabelIDs {
			if l == t.rule.Label {
				msg, err := provider.GetMessage(ctx, ev.MessageID)
				if err != nil {
					return false, Message{}, err
				}
				return true, msg, nil
			}
		}
		return false, Message{}, nil

	case TriggerKindFromWatch:
		if ev.Kind != HistoryMessageAdded {
			return false, Message{}, nil
		}
		msg, err := provider.GetMessage(ctx, ev.MessageID)
		if err != nil {
			return false, Message{}, err
		}
		return strings.Contains(strings.ToLower(msg.From), strings.ToLower(t.rule.From)), msg, nil

	case TriggerKindQueryWatch:
		if ev.Kind != HistoryMessageAdded {
			return false, Message{}, nil
		}
		// query_watch matches via Gmail's search engine. Fetching the
		// message and re-running the query client-side would require
		// a parser; instead we ask Gmail "does this message match the
		// query?" by combining the user's query with rfc822msgid:.
		// The Gmail web search supports this trick.
		// For the v1 simple path we just re-run search and check
		// membership.
		threads, err := provider.SearchThreads(ctx, t.rule.Query+" rfc822msgid:"+ev.MessageID, 1)
		if err != nil {
			return false, Message{}, err
		}
		if len(threads) == 0 {
			return false, Message{}, nil
		}
		msg, err := provider.GetMessage(ctx, ev.MessageID)
		if err != nil {
			return false, Message{}, err
		}
		return true, msg, nil
	}
	return false, Message{}, nil
}

// toTriggerEvent assembles the runtime-facing TriggerEvent for a
// matched message. Goal text is human-readable so it shows up well
// in the runs list.
func (t *gmailTrigger) toTriggerEvent(msg Message) plugins.TriggerEvent {
	goal := fmt.Sprintf("Gmail rule %q matched: %s", t.rule.Name, msg.Subject)
	if msg.From != "" {
		goal = fmt.Sprintf("Gmail rule %q matched message from %s: %s", t.rule.Name, msg.From, msg.Subject)
	}
	return plugins.TriggerEvent{
		ConnectionID: t.conn.ID,
		Kind:         t.rule.Kind,
		Goal:         goal,
		Metadata: map[string]interface{}{
			"rule_name":  t.rule.Name,
			"message_id": msg.ID,
			"thread_id":  msg.ThreadID,
			"from":       msg.From,
			"subject":    msg.Subject,
			"snippet":    msg.Snippet,
			"labels":     msg.Labels,
		},
	}
}
