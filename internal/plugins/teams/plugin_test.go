package teams

import (
	"context"
	"encoding/json"
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
	foundAppID, foundPass := false, false
	for _, c := range m.Requires.Credentials {
		if c.Key == "webhook_secret" && c.Required {
			foundAppID = true
		}
		if c.Key == "app_password" && c.Required {
			foundPass = true
		}
	}
	if !foundAppID || !foundPass {
		t.Fatal("expected webhook_secret (App ID) and app_password credentials")
	}
}

func TestFormatPlanReviewText(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{
		{Title: "Read README", ExpectedCapability: "filesystem.read"},
	}}
	text := formatPlanReviewText("Explain", plan)
	if !strings.Contains(text, "Explain") || !strings.Contains(text, "Read README") {
		t.Fatalf("text: %s", text)
	}
	if !strings.Contains(text, "Approve to execute") {
		t.Fatalf("safe CTA missing: %s", text)
	}
}

func TestPlanReviewCard_DesktopOmitsApprove(t *testing.T) {
	card := planReviewCard("r1", "body", true)
	actions, _ := card["actions"].([]map[string]interface{})
	if len(actions) != 1 {
		t.Fatalf("expected deny only, got %d", len(actions))
	}
	if actions[0]["title"] != "Deny plan" {
		t.Fatalf("title: %v", actions[0]["title"])
	}
	safe := planReviewCard("r2", "body", false)
	safeActions, _ := safe["actions"].([]map[string]interface{})
	if len(safeActions) != 2 {
		t.Fatalf("expected approve+deny, got %d", len(safeActions))
	}
}

func TestParsePlanInvoke(t *testing.T) {
	raw := json.RawMessage(`{"nomi_plan":"approve","run_id":"run-1"}`)
	id, approve, ok := parsePlanInvoke(raw)
	if !ok || !approve || id != "run-1" {
		t.Fatalf("got %q %v %v", id, approve, ok)
	}
	nested := json.RawMessage(`{"action":{"data":{"nomi_plan":"deny","run_id":"run-2"}}}`)
	id, approve, ok = parsePlanInvoke(nested)
	if !ok || approve || id != "run-2" {
		t.Fatalf("nested got %q %v %v", id, approve, ok)
	}
}

func TestReceiveWebhookMessageCreatesNoPanic(t *testing.T) {
	p := NewPlugin(nil, nil, nil, nil, nil, nil, nil, nil)
	body := []byte(`{"type":"message","text":"hi","serviceUrl":"https://smba.example/","conversation":{"id":"c1"},"from":{"id":"u1","name":"Ada"}}`)
	err := p.ReceiveWebhook(context.Background(), "conn-1", body, nil, func(context.Context, plugins.TriggerEvent) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := p.lookupConversation("conn-1", "c1")
	if !ok || ref.ServiceURL != "https://smba.example" {
		t.Fatalf("conversation ref: %#v ok=%v", ref, ok)
	}
}

func TestReceiveWebhookInvokeParses(t *testing.T) {
	p := NewPlugin(nil, nil, nil, nil, nil, nil, nil, nil)
	body := []byte(`{"type":"invoke","name":"adaptiveCard/action","value":{"nomi_plan":"deny","run_id":"r9"},"from":{"id":"u1"},"conversation":{"id":"c1"},"serviceUrl":"https://smba.example/"}`)
	if err := p.ReceiveWebhook(context.Background(), "conn-1", body, nil, nil); err != nil {
		t.Fatal(err)
	}
}
