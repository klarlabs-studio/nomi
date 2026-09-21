package imessage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

func TestManifest(t *testing.T) {
	p := NewPlugin(nil, nil, nil, nil, nil, nil, nil, nil)
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("id: %s", m.ID)
	}
	if m.Cardinality != plugins.ConnectionMulti {
		t.Fatalf("cardinality: %v", m.Cardinality)
	}
	if _, ok := m.Requires.ConfigSchema["api_base_url"]; !ok {
		t.Fatal("api_base_url required")
	}
	found := false
	for _, c := range m.Requires.Credentials {
		if c.Key == "password" && c.Required {
			found = true
		}
	}
	if !found {
		t.Fatal("password credential required")
	}
}

func TestClientSendAndQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("password") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/ping"):
			_, _ = w.Write([]byte(`{"status":200,"message":"pong"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/message/text"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":200,"message":"Success"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/message/query"):
			_, _ = w.Write([]byte(`{"status":200,"message":"Success","data":[{"guid":"g1","text":"hi","isFromMe":false,"handle":{"address":"+1555"},"chats":[{"guid":"iMessage;-;+1555"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cli := newClient(srv.URL, "secret")
	if err := cli.ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cli.sendText(t.Context(), "iMessage;-;+1555", "hello"); err != nil {
		t.Fatal(err)
	}
	msgs, err := cli.queryMessages(t.Context(), 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("query: %v len=%d", err, len(msgs))
	}
	if chatGUIDFromMessage(msgs[0]) != "iMessage;-;+1555" {
		t.Fatalf("chat: %q", chatGUIDFromMessage(msgs[0]))
	}
	if senderFromMessage(msgs[0]) != "+1555" {
		t.Fatalf("sender: %q", senderFromMessage(msgs[0]))
	}
}

func TestFormatAndParsePlan(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{
		{Title: "Read", ExpectedCapability: "filesystem.read"},
	}}
	text := formatPlanReviewText("Goal", plan)
	if !strings.Contains(text, "Reply APPROVE") {
		t.Fatalf("cta: %s", text)
	}
	write := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	desk := formatPlanReviewText("w", write)
	if strings.Contains(desk, "Reply APPROVE to execute") {
		t.Fatalf("desktop omit approve: %s", desk)
	}
	if a, ok := parsePlanReply("YES"); !ok || !a {
		t.Fatal("yes")
	}
	_ = json.RawMessage(nil)
}
