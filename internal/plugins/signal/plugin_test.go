package signal

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
	if _, ok := m.Requires.ConfigSchema["account"]; !ok {
		t.Fatal("account required")
	}
}

func TestClientSendAndReceive(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/send"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"timestamp":1}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v1/receive/"):
			_, _ = w.Write([]byte(`[{"envelope":{"sourceNumber":"+15550001111","sourceName":"Ada","dataMessage":{"message":"hello"}}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cli := newClient(srv.URL, "+15551234567", "tok")
	if err := cli.sendText(t.Context(), "+15550001111", "hi"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth: %q", gotAuth)
	}
	msgs, err := cli.receive(t.Context(), 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("receive: %v len=%d", err, len(msgs))
	}
	if senderFromEnvelope(msgs[0]) != "+15550001111" {
		t.Fatalf("sender: %q", senderFromEnvelope(msgs[0]))
	}
	if conversationFromEnvelope(msgs[0]) != "+15550001111" {
		t.Fatalf("conv: %q", conversationFromEnvelope(msgs[0]))
	}
}

func TestConversationFromGroup(t *testing.T) {
	raw := []byte(`[{"envelope":{"sourceNumber":"+1","dataMessage":{"message":"hi","groupInfo":{"groupId":"abc"}}}}]`)
	var msgs []receiveEnvelope
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	if got := conversationFromEnvelope(msgs[0]); got != "group:abc" {
		t.Fatalf("got %q", got)
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
		t.Fatalf("desktop must omit approve CTA: %s", desk)
	}
	if a, ok := parsePlanReply("APPROVE\nquoted"); !ok || !a {
		t.Fatal("approve")
	}
	if a, ok := parseReactionKey("✅"); !ok || !a {
		t.Fatal("reaction")
	}
}
