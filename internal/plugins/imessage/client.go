package imessage

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

// Client talks to a BlueBubbles Server REST API (macOS iMessage bridge).
type Client struct {
	BaseURL  string
	Password string
	HTTP     *http.Client
}

func newClient(baseURL, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Password: password,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) withAuth(u string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "password=" + url.QueryEscape(c.Password)
}

type sendTextBody struct {
	ChatGUID string `json:"chatGuid"`
	TempGUID string `json:"tempGuid"`
	Message  string `json:"message"`
	Method   string `json:"method,omitempty"`
}

type apiEnvelope struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type bbMessage struct {
	GUID        string  `json:"guid"`
	Text        string  `json:"text"`
	DateCreated float64 `json:"dateCreated"`
	IsFromMe    bool    `json:"isFromMe"`
	Handle      *struct {
		Address string `json:"address"`
	} `json:"handle"`
	Chats []struct {
		GUID string `json:"guid"`
	} `json:"chats"`
}

func (c *Client) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.withAuth(c.BaseURL+"/api/v1/ping"), nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("bluebubbles ping: HTTP %d", res.StatusCode)
	}
	return nil
}

func (c *Client) sendText(ctx context.Context, chatGUID, text string) error {
	payload := sendTextBody{
		ChatGUID: chatGUID,
		TempGUID: fmt.Sprintf("nomi-%d", time.Now().UnixNano()),
		Message:  text,
		Method:   "apple-script",
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.withAuth(c.BaseURL+"/api/v1/message/text"), bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("bluebubbles send: HTTP %d: %s", res.StatusCode, truncateRunes(string(raw), 200))
	}
	return nil
}

// queryMessages fetches recent messages (newest first). afterGUID, when
// set, stops once that guid is seen (exclusive).
func (c *Client) queryMessages(ctx context.Context, limit int) ([]bbMessage, error) {
	if limit <= 0 {
		limit = 25
	}
	body := map[string]interface{}{
		"limit": limit,
		"with":  []string{"chat", "handle"},
		"sort":  "DESC",
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.withAuth(c.BaseURL+"/api/v1/message/query"), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("bluebubbles query: HTTP %d: %s", res.StatusCode, truncateRunes(string(raw), 200))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	// data may be []message or {data: []message}
	var msgs []bbMessage
	if err := json.Unmarshal(env.Data, &msgs); err != nil {
		var wrapped struct {
			Data []bbMessage `json:"data"`
		}
		if err2 := json.Unmarshal(env.Data, &wrapped); err2 != nil {
			return nil, fmt.Errorf("bluebubbles query: parse: %w", err)
		}
		msgs = wrapped.Data
	}
	return msgs, nil
}

func chatGUIDFromMessage(m bbMessage) string {
	if len(m.Chats) > 0 && m.Chats[0].GUID != "" {
		return m.Chats[0].GUID
	}
	if m.Handle != nil && m.Handle.Address != "" {
		return "iMessage;-;" + m.Handle.Address
	}
	return ""
}

func senderFromMessage(m bbMessage) string {
	if m.Handle != nil && m.Handle.Address != "" {
		return m.Handle.Address
	}
	return ""
}
