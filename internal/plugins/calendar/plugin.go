package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/integrations/google"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// Plugin is the Calendar plugin. Tool-only (no channel role, no polling).
// One Connection = one provider account; ProviderKind on the connection
// config selects which backend (Google for v1).
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	oauth       *google.OAuthManager
	secrets     secrets.Store

	// providerOverride lets tests inject a stub Provider without
	// going through the OAuth flow. Production builds leave nil and
	// providerFor constructs a GoogleProvider per call.
	providerOverride func(conn *domain.Connection) (Provider, error)

	mu      sync.RWMutex
	running bool
}

// SetProviderOverride is for tests — replaces providerFor with a
// caller-supplied factory so triggers + tools can run against a
// recorded mock without real OAuth credentials.
func (p *Plugin) SetProviderOverride(fn func(conn *domain.Connection) (Provider, error)) {
	p.providerOverride = fn
}

// NewPlugin constructs the Calendar plugin.
func NewPlugin(
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	oauth *google.OAuthManager,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		connections: conns,
		bindings:    binds,
		oauth:       oauth,
		secrets:     secretStore,
	}
}

// Manifest declares the Calendar plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Calendar",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Read and modify calendar events. Google Calendar in v1; Outlook coming.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"calendar.read",
			"calendar.write",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{Name: "calendar.list_upcoming", Capability: "calendar.read", RequiresConnection: true,
					Description: "List calendar events between from and to. Inputs: connection_id, calendar_id?, from (RFC3339), to (RFC3339), limit?"},
				{Name: "calendar.create_event", Capability: "calendar.write", RequiresConnection: true,
					Description: "Create a new calendar event. Inputs: connection_id, calendar_id?, title, start (RFC3339), end (RFC3339), description?, attendees?[], location?"},
				{Name: "calendar.update_event", Capability: "calendar.write", RequiresConnection: true,
					Description: "Update an existing event. Inputs: connection_id, calendar_id?, event_id, plus any fields to change."},
				{Name: "calendar.delete_event", Capability: "calendar.write", RequiresConnection: true,
					Description: "Delete an event by id. Idempotent: deleting an already-gone event is not an error."},
				{Name: "calendar.find_free_slots", Capability: "calendar.read", RequiresConnection: true,
					Description: "Find contiguous free blocks of at least duration_minutes between from and to across the given calendar ids."},
			},
			Triggers: []plugins.TriggerContribution{
				{Kind: TriggerKindPreMeeting, EventType: "calendar.pre_meeting",
					Description: "Fire a run N minutes before an event starts. Rule shape: {kind: calendar.pre_meeting, name, lead_minutes, calendar_id?}."},
				{Kind: TriggerKindEventCreated, EventType: "calendar.event_created",
					Description: "Fire a run when a new event appears in the calendar within the next 24h. Rule shape: {kind: calendar.event_created, name, calendar_id?}."},
				{Kind: TriggerKindEventCancelled, EventType: "calendar.event_cancelled",
					Description: "Fire a run when an upcoming event disappears (cancelled or deleted). Rule shape: {kind: calendar.event_cancelled, name, calendar_id?}."},
			},
		},
		Requires: plugins.Requirements{
			ConfigSchema: map[string]plugins.ConfigField{
				"provider": {
					Type: "string", Label: "Provider", Required: true, Default: "google",
					Description: `Which calendar backend: "google" (v1) or "outlook" (future).`,
				},
				"account_id": {
					Type: "string", Label: "Account ID", Required: true,
					Description: "Google OAuth account id (returned from the device-flow linking step).",
				},
				"client_id": {
					Type: "string", Label: "OAuth Client ID", Required: true,
					Description: "OAuth client id the account was linked under.",
				},
			},
			NetworkAllowlist: []string{"www.googleapis.com", "oauth2.googleapis.com"},
		},
	}
}

// Configure is a no-op.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start marks the plugin running. Calendar has no long-running worker —
// tool calls are synchronous.
func (p *Plugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	return nil
}

// Stop unwinds the running flag.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

// Status returns plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// Tools implements plugins.ToolProvider.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&listUpcomingTool{plugin: p},
		&createEventTool{plugin: p},
		&updateEventTool{plugin: p},
		&deleteEventTool{plugin: p},
		&findFreeSlotsTool{plugin: p},
	}
}

// providerFor returns the backend client for a given Connection. v1
// only implements Google; unknown providers return an explicit error
// so the tool call fails loud rather than silently misbehaving.
func (p *Plugin) providerFor(conn *domain.Connection) (Provider, error) {
	if p.providerOverride != nil {
		return p.providerOverride(conn)
	}
	providerKind, _ := conn.Config["provider"].(string)
	if providerKind == "" {
		providerKind = string(ProviderGoogle)
	}
	accountID, _ := conn.Config["account_id"].(string)
	clientID, _ := conn.Config["client_id"].(string)
	switch ProviderKind(providerKind) {
	case ProviderGoogle:
		if p.oauth == nil {
			return nil, fmt.Errorf("google oauth manager not configured")
		}
		if accountID == "" || clientID == "" {
			return nil, fmt.Errorf("google calendar requires account_id + client_id in config")
		}
		return NewGoogleProvider(p.oauth, clientID, accountID), nil
	case ProviderOutlook:
		return nil, fmt.Errorf("outlook calendar provider not implemented in v1")
	default:
		return nil, fmt.Errorf("unknown calendar provider %q", providerKind)
	}
}

// --- tools ---

type calendarToolBase struct {
	plugin *Plugin
	name   string
	cap    string
}

func (t *calendarToolBase) Name() string       { return t.name }
func (t *calendarToolBase) Capability() string { return t.cap }

// resolveProvider does the common validation dance: connection_id present,
// binding exists for the calling assistant (role=tool), connection is
// enabled, provider constructible. Returns the Connection too so tools
// can read its config.
func (t *calendarToolBase) resolveProvider(input map[string]interface{}) (Provider, *domain.Connection, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, nil, fmt.Errorf("%s: connection_id is required", t.name)
	}
	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: binding check failed: %w", t.name, err)
		}
		if !ok {
			return nil, nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}
	conn, err := t.plugin.connections.GetByID(connectionID)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", t.name, err)
	}
	if !conn.Enabled {
		return nil, nil, fmt.Errorf("%s: connection %s is disabled", t.name, connectionID)
	}
	provider, err := t.plugin.providerFor(conn)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", t.name, err)
	}
	return provider, conn, nil
}

type listUpcomingTool struct{ plugin *Plugin }

func (t *listUpcomingTool) Name() string       { return "calendar.list_upcoming" }
func (t *listUpcomingTool) Capability() string { return "calendar.read" }

func (t *listUpcomingTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	base := calendarToolBase{plugin: t.plugin, name: t.Name(), cap: t.Capability()}
	provider, _, err := base.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	calendarID, _ := input["calendar_id"].(string)
	from, err := parseTime(input, "from", time.Now())
	if err != nil {
		return nil, err
	}
	to, err := parseTime(input, "to", from.Add(7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	limit := parseInt(input, "limit", 25)
	events, err := provider.ListUpcoming(ctx, calendarID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name(), err)
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, eventToMap(e))
	}
	return map[string]interface{}{"events": out, "count": len(out)}, nil
}

type createEventTool struct{ plugin *Plugin }

func (t *createEventTool) Name() string       { return "calendar.create_event" }
func (t *createEventTool) Capability() string { return "calendar.write" }

func (t *createEventTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	base := calendarToolBase{plugin: t.plugin, name: t.Name(), cap: t.Capability()}
	provider, _, err := base.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	title, _ := input["title"].(string)
	if title == "" {
		return nil, fmt.Errorf("%s: title is required", t.Name())
	}
	start, err := parseTime(input, "start", time.Time{})
	if err != nil || start.IsZero() {
		return nil, fmt.Errorf("%s: start is required (RFC3339)", t.Name())
	}
	end, err := parseTime(input, "end", start.Add(time.Hour))
	if err != nil {
		return nil, err
	}
	calendarID, _ := input["calendar_id"].(string)
	event := Event{
		Title:       title,
		Description: stringFromInput(input, "description"),
		Location:    stringFromInput(input, "location"),
		Start:       start,
		End:         end,
		Attendees:   stringSliceFromInput(input, "attendees"),
	}
	created, err := provider.CreateEvent(ctx, calendarID, event)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name(), err)
	}
	return eventToMap(created), nil
}

type updateEventTool struct{ plugin *Plugin }

func (t *updateEventTool) Name() string       { return "calendar.update_event" }
func (t *updateEventTool) Capability() string { return "calendar.write" }

func (t *updateEventTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	base := calendarToolBase{plugin: t.plugin, name: t.Name(), cap: t.Capability()}
	provider, _, err := base.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	eventID, _ := input["event_id"].(string)
	if eventID == "" {
		return nil, fmt.Errorf("%s: event_id is required", t.Name())
	}
	calendarID, _ := input["calendar_id"].(string)
	event := Event{
		Title:       stringFromInput(input, "title"),
		Description: stringFromInput(input, "description"),
		Location:    stringFromInput(input, "location"),
		Attendees:   stringSliceFromInput(input, "attendees"),
	}
	if s, err := parseTime(input, "start", time.Time{}); err == nil && !s.IsZero() {
		event.Start = s
	}
	if e, err := parseTime(input, "end", time.Time{}); err == nil && !e.IsZero() {
		event.End = e
	}
	updated, err := provider.UpdateEvent(ctx, calendarID, eventID, event)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name(), err)
	}
	return eventToMap(updated), nil
}

type deleteEventTool struct{ plugin *Plugin }

func (t *deleteEventTool) Name() string       { return "calendar.delete_event" }
func (t *deleteEventTool) Capability() string { return "calendar.write" }

func (t *deleteEventTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	base := calendarToolBase{plugin: t.plugin, name: t.Name(), cap: t.Capability()}
	provider, _, err := base.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	eventID, _ := input["event_id"].(string)
	if eventID == "" {
		return nil, fmt.Errorf("%s: event_id is required", t.Name())
	}
	calendarID, _ := input["calendar_id"].(string)
	if err := provider.DeleteEvent(ctx, calendarID, eventID); err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name(), err)
	}
	return map[string]interface{}{"status": "deleted"}, nil
}

type findFreeSlotsTool struct{ plugin *Plugin }

func (t *findFreeSlotsTool) Name() string       { return "calendar.find_free_slots" }
func (t *findFreeSlotsTool) Capability() string { return "calendar.read" }

func (t *findFreeSlotsTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	base := calendarToolBase{plugin: t.plugin, name: t.Name(), cap: t.Capability()}
	provider, _, err := base.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	ids := stringSliceFromInput(input, "calendar_ids")
	if len(ids) == 0 {
		ids = []string{"primary"}
	}
	from, err := parseTime(input, "from", time.Now())
	if err != nil {
		return nil, err
	}
	to, err := parseTime(input, "to", from.Add(7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	durationMinutes := parseInt(input, "duration_minutes", 30)
	slots, err := provider.FindFreeSlots(ctx, ids, from, to, time.Duration(durationMinutes)*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Name(), err)
	}
	out := make([]map[string]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, map[string]string{
			"start": s.Start.UTC().Format(time.RFC3339),
			"end":   s.End.UTC().Format(time.RFC3339),
		})
	}
	return map[string]interface{}{"slots": out, "count": len(out)}, nil
}

// --- input coercion helpers ---

func parseTime(input map[string]interface{}, key string, fallback time.Time) (time.Time, error) {
	raw, ok := input[key].(string)
	if !ok || raw == "" {
		return fallback, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339: %w", key, err)
	}
	return t, nil
}

func parseInt(input map[string]interface{}, key string, fallback int) int {
	if v, ok := input[key].(float64); ok {
		return int(v)
	}
	if s, ok := input[key].(string); ok {
		var n int
		_, _ = fmt.Sscanf(s, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return fallback
}

func stringFromInput(input map[string]interface{}, key string) string {
	s, _ := input[key].(string)
	return s
}

func stringSliceFromInput(input map[string]interface{}, key string) []string {
	switch v := input[key].(type) {
	case nil:
		return nil
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

func eventToMap(e Event) map[string]any {
	out := map[string]any{
		"id":          e.ID,
		"title":       e.Title,
		"description": e.Description,
		"location":    e.Location,
		"attendees":   e.Attendees,
	}
	if !e.Start.IsZero() {
		out["start"] = e.Start.UTC().Format(time.RFC3339)
	}
	if !e.End.IsZero() {
		out["end"] = e.End.UTC().Format(time.RFC3339)
	}
	if len(e.ProviderData) > 0 {
		out["provider_data"] = e.ProviderData
	}
	return out
}

// Compile-time checks.
var _ plugins.Plugin = (*Plugin)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ tools.Tool = (*listUpcomingTool)(nil)
var _ tools.Tool = (*createEventTool)(nil)
var _ tools.Tool = (*updateEventTool)(nil)
var _ tools.Tool = (*deleteEventTool)(nil)
var _ tools.Tool = (*findFreeSlotsTool)(nil)
