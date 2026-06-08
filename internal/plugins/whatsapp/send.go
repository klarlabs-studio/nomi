package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GraphAPIBase is the WhatsApp Cloud API endpoint root. Pinned to v18.0
// because the message shape is stable through that version; bumping
// requires re-testing the body schema for breaking changes. Declared
// as a package var (not const) so tests can repoint SendText at a
// httptest server without monkey-patching the http transport.
var GraphAPIBase = "https://graph.facebook.com/v18.0"

// SendTextOptions are the inputs needed to deliver a single text message.
type SendTextOptions struct {
	PhoneNumberID string // Cloud API phone number ID; routes the outbound message to the right WABA number
	AccessToken   string // System User access token
	To            string // recipient in E.164 (e.g. "+14155551234")
	Body          string // message body; Cloud API caps at 4096 chars
}

// SendTextResponse is the relevant subset of the Cloud API response.
type SendTextResponse struct {
	MessagingProduct string `json:"messaging_product"`
	Contacts         []struct {
		Input string `json:"input"`
		WAID  string `json:"wa_id"`
	} `json:"contacts"`
	Messages []struct {
		ID            string `json:"id"`
		MessageStatus string `json:"message_status"`
	} `json:"messages"`
}

// SendText posts a single text message through the Cloud API. Returns the
// parsed response on success; on non-2xx returns a formatted error
// including the upstream body so operators can diagnose webhook
// credential or phone-number mismatches.
func SendText(ctx context.Context, client *http.Client, opts SendTextOptions) (*SendTextResponse, error) {
	if opts.PhoneNumberID == "" {
		return nil, fmt.Errorf("whatsapp send: phone_number_id is required")
	}
	if opts.AccessToken == "" {
		return nil, fmt.Errorf("whatsapp send: access_token is required")
	}
	if opts.To == "" {
		return nil, fmt.Errorf("whatsapp send: to is required")
	}
	if opts.Body == "" {
		return nil, fmt.Errorf("whatsapp send: body is required")
	}

	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"to":                opts.To,
		"type":              "text",
		"text":              map[string]string{"body": opts.Body},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("whatsapp send: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/%s/messages", GraphAPIBase, opts.PhoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("whatsapp send: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+opts.AccessToken)

	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whatsapp send: http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("whatsapp send: status %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed SendTextResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("whatsapp send: parse response: %w", err)
	}
	return &parsed, nil
}
