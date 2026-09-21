package signal

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

// Client talks to a signal-cli-rest-api sidecar (bbernhard/signal-cli-rest-api).
type Client struct {
	BaseURL string
	Account string // E.164 account number registered in signal-cli
	Token   string // optional API auth token
	HTTP    *http.Client
}

func newClient(baseURL, account, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Account: strings.TrimSpace(account),
		Token:   token,
		HTTP:    &http.Client{Timeout: 90 * time.Second},
	}
}

type sendRequest struct {
	Message    string   `json:"message"`
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
}

type receiveEnvelope struct {
	Envelope *struct {
		Source       string `json:"source"`
		SourceNumber string `json:"sourceNumber"`
		SourceName   string `json:"sourceName"`
		SourceUUID   string `json:"sourceUuid"`
		DataMessage  *struct {
			Message   string `json:"message"`
			Timestamp int64  `json:"timestamp"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
			} `json:"groupInfo"`
			Reaction *struct {
				Emoji               string `json:"emoji"`
				TargetAuthor        string `json:"targetAuthor"`
				TargetAuthorNumber  string `json:"targetAuthorNumber"`
				TargetSentTimestamp int64  `json:"targetSentTimestamp"`
			} `json:"reaction"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

func (c *Client) sendText(ctx context.Context, recipient, body string) error {
	payload := sendRequest{
		Message:    body,
		Number:     c.Account,
		Recipients: []string{recipient},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v2/send", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("signal send: HTTP %d: %s", res.StatusCode, truncateRunes(string(raw), 200))
	}
	return nil
}

func (c *Client) receive(ctx context.Context, timeoutSec int) ([]receiveEnvelope, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	path := fmt.Sprintf("/v1/receive/%s?timeout=%d&max_messages=50&ignore_stories=true",
		url.PathEscape(c.Account), timeoutSec)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
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
		return nil, fmt.Errorf("signal receive: HTTP %d: %s", res.StatusCode, truncateRunes(string(raw), 200))
	}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var out []receiveEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("signal receive: parse: %w", err)
	}
	return out, nil
}

func senderFromEnvelope(ev receiveEnvelope) string {
	if ev.Envelope == nil {
		return ""
	}
	if ev.Envelope.SourceNumber != "" {
		return ev.Envelope.SourceNumber
	}
	return ev.Envelope.Source
}

func conversationFromEnvelope(ev receiveEnvelope) string {
	if ev.Envelope == nil || ev.Envelope.DataMessage == nil {
		return senderFromEnvelope(ev)
	}
	if gi := ev.Envelope.DataMessage.GroupInfo; gi != nil && gi.GroupID != "" {
		return "group:" + gi.GroupID
	}
	return senderFromEnvelope(ev)
}
