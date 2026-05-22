package whatsapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/plugins"
)

func TestManifestShape(t *testing.T) {
	p := &Plugin{}
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("ID mismatch: %q", m.ID)
	}
	if m.Cardinality != plugins.ConnectionMulti {
		t.Fatal("expected ConnectionMulti cardinality")
	}
	if len(m.Contributes.Channels) != 1 || m.Contributes.Channels[0].Kind != "whatsapp" {
		t.Fatalf("channel contribution missing: %+v", m.Contributes.Channels)
	}
	foundTool := false
	for _, tc := range m.Contributes.Tools {
		if tc.Name == "whatsapp.send_message" && tc.Capability == "whatsapp.send" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatal("whatsapp.send_message tool contribution missing")
	}
}

func TestReceiveWebhookFiresOnTextMessage(t *testing.T) {
	p := &Plugin{healthPerConn: map[string]*plugins.ConnectionHealth{}}

	body := []byte(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"id": "wabaid",
			"changes": [{
				"field": "messages",
				"value": {
					"messaging_product": "whatsapp",
					"metadata": {"display_phone_number": "+1555", "phone_number_id": "phn-1"},
					"contacts": [{"profile": {"name": "Alice"}, "wa_id": "+14155551234"}],
					"messages": [{
						"from": "+14155551234",
						"id": "wamid.test",
						"timestamp": "1700000000",
						"type": "text",
						"text": {"body": "hello bot"}
					}]
				}
			}]
		}]
	}`)

	fires := []plugins.TriggerEvent{}
	err := p.ReceiveWebhook(context.Background(), "conn-1", body, nil, func(_ context.Context, ev plugins.TriggerEvent) error {
		fires = append(fires, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fires) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(fires))
	}
	ev := fires[0]
	if ev.ConnectionID != "conn-1" {
		t.Errorf("ConnectionID: got %q", ev.ConnectionID)
	}
	if ev.Kind != "whatsapp" {
		t.Errorf("Kind: got %q", ev.Kind)
	}
	if ev.Metadata["from"] != "+14155551234" {
		t.Errorf("from metadata: got %v", ev.Metadata["from"])
	}
	if ev.Metadata["text"] != "hello bot" {
		t.Errorf("text metadata: got %v", ev.Metadata["text"])
	}
	if ev.Metadata["profile_name"] != "Alice" {
		t.Errorf("profile_name metadata: got %v", ev.Metadata["profile_name"])
	}
}

func TestReceiveWebhookIgnoresNonTextMessages(t *testing.T) {
	p := &Plugin{healthPerConn: map[string]*plugins.ConnectionHealth{}}

	body := []byte(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"messages": [{
						"from": "+14155551234",
						"id": "wamid.img",
						"type": "image",
						"text": {"body": ""}
					}]
				}
			}]
		}]
	}`)
	fires := 0
	err := p.ReceiveWebhook(context.Background(), "conn-1", body, nil, func(context.Context, plugins.TriggerEvent) error {
		fires++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fires != 0 {
		t.Fatalf("expected 0 fires on non-text message, got %d", fires)
	}
}

func TestReceiveWebhookRejectsUnknownObject(t *testing.T) {
	p := &Plugin{healthPerConn: map[string]*plugins.ConnectionHealth{}}
	body := []byte(`{"object": "page", "entry": []}`)
	fires := 0
	if err := p.ReceiveWebhook(context.Background(), "conn-1", body, nil, func(context.Context, plugins.TriggerEvent) error {
		fires++
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fires != 0 {
		t.Fatal("expected 0 fires on unknown object")
	}
}

func TestSendTextSuccess(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-abc" {
			t.Errorf("Authorization: got %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"messaging_product": "whatsapp",
			"contacts": [{"input": "+14155551234", "wa_id": "+14155551234"}],
			"messages": [{"id": "wamid.outgoing"}]
		}`))
	}))
	defer srv.Close()

	// Redirect the Graph API base to our test server by monkey-pointing
	// through the SendTextOptions PhoneNumberID — easier: use the
	// client.Transport to rewrite the host. But that's heavy. Instead, send
	// a test-only request directly via http.Client to assert the body
	// shape is right via our SendText function's contract. We can't easily
	// override the URL without making the constant a var; do that.
	prev := GraphAPIBase
	GraphAPIBase = srv.URL
	t.Cleanup(func() { GraphAPIBase = prev })

	resp, err := SendText(context.Background(), srv.Client(), SendTextOptions{
		PhoneNumberID: "phn-1",
		AccessToken:   "tok-abc",
		To:            "+14155551234",
		Body:          "hi",
	})
	if err != nil {
		t.Fatalf("SendText failed: %v", err)
	}
	if resp.Messages[0].ID != "wamid.outgoing" {
		t.Fatalf("unexpected message id: %+v", resp)
	}
	if got, _ := receivedBody["messaging_product"].(string); got != "whatsapp" {
		t.Errorf("messaging_product: got %v", got)
	}
	if got, _ := receivedBody["to"].(string); got != "+14155551234" {
		t.Errorf("to: got %v", got)
	}
}

func TestSendTextRejectsMissingInputs(t *testing.T) {
	cases := []struct {
		name string
		opts SendTextOptions
	}{
		{"no phone", SendTextOptions{AccessToken: "t", To: "+1", Body: "x"}},
		{"no token", SendTextOptions{PhoneNumberID: "p", To: "+1", Body: "x"}},
		{"no to", SendTextOptions{PhoneNumberID: "p", AccessToken: "t", Body: "x"}},
		{"no body", SendTextOptions{PhoneNumberID: "p", AccessToken: "t", To: "+1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SendText(context.Background(), nil, c.opts); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}
