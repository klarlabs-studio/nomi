package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"go.klarlabs.de/nomi/internal/integrations/google"
)

// GoogleProvider implements Provider against the Google Calendar v3 REST
// API. We call the REST API directly (rather than pulling in
// google.golang.org/api/calendar/v3) to keep the dependency footprint
// small — the surface we need is modest.
type GoogleProvider struct {
	oauth     *google.OAuthManager
	clientID  string
	accountID string
	http      *http.Client
	apiBase   string // overridable for tests
}

// NewGoogleProvider wires a Google Calendar provider for one Connection.
// The OAuthManager must already hold a refresh token for accountID under
// the google.OAuthManager's refreshTokenKey scheme — that's the state
// left behind by the device-flow endpoints in integrations/google.
func NewGoogleProvider(oauth *google.OAuthManager, clientID, accountID string) *GoogleProvider {
	return &GoogleProvider{
		oauth:     oauth,
		clientID:  clientID,
		accountID: accountID,
		http:      &http.Client{Timeout: 30 * time.Second},
		apiBase:   "https://www.googleapis.com/calendar/v3",
	}
}

// ListUpcoming fetches events from a calendar between from and to. Uses
// singleEvents=true so recurring instances expand into individual
// Events the tool can surface.
func (g *GoogleProvider) ListUpcoming(ctx context.Context, calendarID string, from, to time.Time, limit int) ([]Event, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	q := url.Values{}
	q.Set("timeMin", from.UTC().Format(time.RFC3339))
	q.Set("timeMax", to.UTC().Format(time.RFC3339))
	q.Set("singleEvents", "true")
	q.Set("orderBy", "startTime")
	if limit > 0 {
		q.Set("maxResults", fmt.Sprintf("%d", limit))
	}

	endpoint := fmt.Sprintf("%s/calendars/%s/events?%s", g.apiBase, url.PathEscape(calendarID), q.Encode())
	var resp struct {
		Items []googleEvent `json:"items"`
	}
	if err := g.doGET(ctx, endpoint, &resp); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	out := make([]Event, 0, len(resp.Items))
	for _, item := range resp.Items {
		out = append(out, item.toEvent())
	}
	return out, nil
}

// CreateEvent inserts a new event on the named calendar.
func (g *GoogleProvider) CreateEvent(ctx context.Context, calendarID string, event Event) (Event, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	body := fromEvent(event)
	endpoint := fmt.Sprintf("%s/calendars/%s/events", g.apiBase, url.PathEscape(calendarID))
	var resp googleEvent
	if err := g.doJSON(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		return Event{}, fmt.Errorf("create event: %w", err)
	}
	return resp.toEvent(), nil
}

// UpdateEvent patches an existing event. Uses PATCH so the caller can
// omit fields they don't want to change.
func (g *GoogleProvider) UpdateEvent(ctx context.Context, calendarID, eventID string, event Event) (Event, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	if eventID == "" {
		return Event{}, fmt.Errorf("event_id is required")
	}
	body := fromEvent(event)
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s",
		g.apiBase, url.PathEscape(calendarID), url.PathEscape(eventID))
	var resp googleEvent
	if err := g.doJSON(ctx, http.MethodPatch, endpoint, body, &resp); err != nil {
		return Event{}, fmt.Errorf("update event: %w", err)
	}
	return resp.toEvent(), nil
}

// DeleteEvent removes an event. 404 is treated as success for idempotency.
func (g *GoogleProvider) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	if calendarID == "" {
		calendarID = "primary"
	}
	if eventID == "" {
		return fmt.Errorf("event_id is required")
	}
	endpoint := fmt.Sprintf("%s/calendars/%s/events/%s",
		g.apiBase, url.PathEscape(calendarID), url.PathEscape(eventID))
	token, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("delete event: status %d", resp.StatusCode)
	}
	return nil
}

// FindFreeSlots walks the busy periods Google returns from /freeBusy
// and emits the inverse — the contiguous gaps of at least `duration`.
// Handles the multi-calendar case by merging busy ranges across every
// calendarID supplied.
func (g *GoogleProvider) FindFreeSlots(ctx context.Context, calendarIDs []string, from, to time.Time, duration time.Duration) ([]FreeSlot, error) {
	if len(calendarIDs) == 0 {
		calendarIDs = []string{"primary"}
	}
	if duration <= 0 {
		return nil, fmt.Errorf("duration must be positive")
	}

	items := make([]map[string]string, 0, len(calendarIDs))
	for _, id := range calendarIDs {
		items = append(items, map[string]string{"id": id})
	}
	body := map[string]any{
		"timeMin": from.UTC().Format(time.RFC3339),
		"timeMax": to.UTC().Format(time.RFC3339),
		"items":   items,
	}
	endpoint := fmt.Sprintf("%s/freeBusy", g.apiBase)

	var resp struct {
		Calendars map[string]struct {
			Busy []struct {
				Start time.Time `json:"start"`
				End   time.Time `json:"end"`
			} `json:"busy"`
		} `json:"calendars"`
	}
	if err := g.doJSON(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		return nil, fmt.Errorf("free-busy: %w", err)
	}

	// Flatten busy ranges across all calendars, sort + merge, then
	// invert against [from, to] to get free blocks of at least
	// duration.
	type busy struct{ start, end time.Time }
	var busyRanges []busy
	for _, c := range resp.Calendars {
		for _, b := range c.Busy {
			busyRanges = append(busyRanges, busy{start: b.Start, end: b.End})
		}
	}
	sort.Slice(busyRanges, func(i, j int) bool { return busyRanges[i].start.Before(busyRanges[j].start) })

	// Merge overlapping busy ranges.
	merged := make([]busy, 0, len(busyRanges))
	for _, b := range busyRanges {
		if len(merged) == 0 || b.start.After(merged[len(merged)-1].end) {
			merged = append(merged, b)
			continue
		}
		if b.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = b.end
		}
	}

	cursor := from
	out := make([]FreeSlot, 0)
	for _, b := range merged {
		if b.start.After(cursor) && b.start.Sub(cursor) >= duration {
			out = append(out, FreeSlot{Start: cursor, End: b.start})
		}
		if b.end.After(cursor) {
			cursor = b.end
		}
	}
	if to.Sub(cursor) >= duration {
		out = append(out, FreeSlot{Start: cursor, End: to})
	}
	return out, nil
}

// accessToken refreshes and returns a bearer token for API calls.
func (g *GoogleProvider) accessToken(ctx context.Context) (string, error) {
	token, err := g.oauth.GetToken(ctx, g.accountID, g.clientID)
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

// doGET executes a GET request and decodes the JSON body into target.
func (g *GoogleProvider) doGET(ctx context.Context, endpoint string, target any) error {
	token, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

// doJSON executes a JSON POST/PATCH and decodes the JSON body.
func (g *GoogleProvider) doJSON(ctx context.Context, method, endpoint string, body any, target any) error {
	token, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if target != nil {
		return json.NewDecoder(resp.Body).Decode(target)
	}
	return nil
}

// googleEvent is the wire shape for a Google Calendar event. We only
// map the fields we round-trip; unknown fields deserialize into
// ProviderData.
type googleEvent struct {
	ID          string `json:"id,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	Start       struct {
		DateTime time.Time `json:"dateTime,omitempty"`
		TimeZone string    `json:"timeZone,omitempty"`
	} `json:"start,omitempty"`
	End struct {
		DateTime time.Time `json:"dateTime,omitempty"`
		TimeZone string    `json:"timeZone,omitempty"`
	} `json:"end,omitempty"`
	Attendees []struct {
		Email string `json:"email"`
	} `json:"attendees,omitempty"`
	HangoutLink string `json:"hangoutLink,omitempty"`
}

func (e googleEvent) toEvent() Event {
	attendees := make([]string, 0, len(e.Attendees))
	for _, a := range e.Attendees {
		if a.Email != "" {
			attendees = append(attendees, a.Email)
		}
	}
	provData := map[string]any{}
	if e.HangoutLink != "" {
		provData["hangout_link"] = e.HangoutLink
	}
	return Event{
		ID:           e.ID,
		Title:        e.Summary,
		Description:  e.Description,
		Location:     e.Location,
		Start:        e.Start.DateTime,
		End:          e.End.DateTime,
		Attendees:    attendees,
		ProviderData: provData,
	}
}

func fromEvent(e Event) map[string]any {
	body := map[string]any{}
	if e.Title != "" {
		body["summary"] = e.Title
	}
	if e.Description != "" {
		body["description"] = e.Description
	}
	if e.Location != "" {
		body["location"] = e.Location
	}
	if !e.Start.IsZero() {
		body["start"] = map[string]any{"dateTime": e.Start.UTC().Format(time.RFC3339)}
	}
	if !e.End.IsZero() {
		body["end"] = map[string]any{"dateTime": e.End.UTC().Format(time.RFC3339)}
	}
	if len(e.Attendees) > 0 {
		att := make([]map[string]string, 0, len(e.Attendees))
		for _, a := range e.Attendees {
			att = append(att, map[string]string{"email": a})
		}
		body["attendees"] = att
	}
	return body
}
