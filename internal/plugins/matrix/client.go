package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal Matrix Client-Server API client (v3). Kept thin —
// no mautrix dependency — so homeserver egress stays stdlib HTTP like
// the WhatsApp Graph path.
type Client struct {
	Homeserver  string
	AccessToken string
	UserID      string
	HTTP        *http.Client
}

func newClient(homeserver, accessToken string) *Client {
	hs := strings.TrimRight(strings.TrimSpace(homeserver), "/")
	return &Client{
		Homeserver:  hs,
		AccessToken: accessToken,
		HTTP:        &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *Client) whoami(ctx context.Context) (string, error) {
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, &out); err != nil {
		return "", err
	}
	if out.UserID == "" {
		return "", fmt.Errorf("matrix whoami: empty user_id")
	}
	c.UserID = out.UserID
	return out.UserID, nil
}

type syncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join   map[string]syncRoom `json:"join"`
		Invite map[string]syncRoom `json:"invite"`
	} `json:"rooms"`
}

type syncRoom struct {
	Timeline struct {
		Events []rawEvent `json:"events"`
	} `json:"timeline"`
	InviteState struct {
		Events []rawEvent `json:"events"`
	} `json:"invite_state"`
}

type rawEvent struct {
	Type           string          `json:"type"`
	EventID        string          `json:"event_id"`
	Sender         string          `json:"sender"`
	OriginServerTS int64           `json:"origin_server_ts"`
	Content        json.RawMessage `json:"content"`
}

type textMessageContent struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
}

type reactionContent struct {
	RelatesTo struct {
		EventID string `json:"event_id"`
		RelType string `json:"rel_type"`
		Key     string `json:"key"`
	} `json:"m.relates_to"`
}

func (c *Client) sync(ctx context.Context, since string, timeoutMS int) (*syncResponse, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if timeoutMS > 0 {
		q.Set("timeout", fmt.Sprintf("%d", timeoutMS))
	}
	path := "/_matrix/client/v3/sync"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out syncResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) sendText(ctx context.Context, roomID, body string) (string, error) {
	payload := map[string]string{
		"msgtype": "m.text",
		"body":    body,
	}
	return c.sendEvent(ctx, roomID, "m.room.message", payload)
}

func (c *Client) sendEvent(ctx context.Context, roomID, eventType string, content any) (string, error) {
	txn := fmt.Sprintf("nomi-%d", time.Now().UnixNano())
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/%s/%s",
		url.PathEscape(roomID), url.PathEscape(eventType), url.PathEscape(txn))
	var out struct {
		EventID string `json:"event_id"`
	}
	if err := c.do(ctx, http.MethodPut, path, content, &out); err != nil {
		return "", err
	}
	return out.EventID, nil
}

func (c *Client) joinRoom(ctx context.Context, roomID string) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", url.PathEscape(roomID))
	return c.do(ctx, http.MethodPost, path, map[string]any{}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Homeserver+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("matrix %s %s: HTTP %d: %s", method, path, res.StatusCode, truncateRunes(string(raw), 200))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func parseTextBody(content json.RawMessage) (string, bool) {
	var c textMessageContent
	if err := json.Unmarshal(content, &c); err != nil {
		return "", false
	}
	if c.MsgType != "" && c.MsgType != "m.text" && c.MsgType != "m.notice" {
		return "", false
	}
	body := strings.TrimSpace(c.Body)
	return body, body != ""
}

func parseReaction(content json.RawMessage) (eventID, key string, ok bool) {
	var c reactionContent
	if err := json.Unmarshal(content, &c); err != nil {
		return "", "", false
	}
	if c.RelatesTo.RelType != "m.annotation" || c.RelatesTo.EventID == "" || c.RelatesTo.Key == "" {
		return "", "", false
	}
	return c.RelatesTo.EventID, c.RelatesTo.Key, true
}
