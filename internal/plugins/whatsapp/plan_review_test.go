package whatsapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

func TestFormatPlanReviewText_IncludesGoalAndSteps(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{
		{Title: "Read README", ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read", Order: 0},
		{Title: "Summarize", ExpectedTool: "llm.chat", ExpectedCapability: "llm.chat", Order: 1},
	}}
	text := formatPlanReviewText("Explain the repo", plan)
	if !strings.Contains(text, "Explain the repo") {
		t.Fatalf("goal missing: %s", text)
	}
	if !strings.Contains(text, "Read README") || !strings.Contains(text, "filesystem.read") {
		t.Fatalf("step missing: %s", text)
	}
	if !strings.Contains(text, "Approve to execute") {
		t.Fatalf("safe-plan CTA missing: %s", text)
	}
}

func TestPlanRequiresDesktopReview_WriteAndPatch(t *testing.T) {
	write := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.write", ExpectedCapability: "filesystem.write",
	}}}
	if !planRequiresDesktopReview(write) {
		t.Fatal("filesystem.write must require desktop")
	}
	safe := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "filesystem.read", ExpectedCapability: "filesystem.read",
	}}}
	if planRequiresDesktopReview(safe) {
		t.Fatal("read-only plan must not require desktop")
	}
}

func TestPlanRequiresDesktopReview_MutatingMCP(t *testing.T) {
	plan := &domain.Plan{Steps: []domain.StepDefinition{{
		ExpectedTool: "mcp.docs.write_file", ExpectedCapability: "mcp.docs.write_file",
	}}}
	if !planRequiresDesktopReview(plan) {
		t.Fatal("mutating MCP must require desktop")
	}
}

func TestPlanReviewButtons_SafeHasApproveAndDeny(t *testing.T) {
	btns := planReviewButtons("run-1", false)
	if len(btns) != 2 {
		t.Fatalf("want 2 buttons, got %d", len(btns))
	}
	if btns[0].ID != callbackPlanApprove+"run-1" || btns[1].ID != callbackPlanDeny+"run-1" {
		t.Fatalf("unexpected ids: %+v", btns)
	}
	for _, b := range btns {
		if len([]rune(b.Title)) > 20 {
			t.Fatalf("title over 20 runes: %q", b.Title)
		}
	}
}

func TestPlanReviewButtons_DesktopOnlyOmitsApprove(t *testing.T) {
	btns := planReviewButtons("run-2", true)
	if len(btns) != 1 || btns[0].ID != callbackPlanDeny+"run-2" {
		t.Fatalf("desktop plan should only offer deny: %+v", btns)
	}
}

func TestReceiveWebhookInteractiveButtonReply(t *testing.T) {
	p := &Plugin{
		healthPerConn: map[string]*plugins.ConnectionHealth{},
		planMsg:       map[string]planMsgRef{},
	}
	body := []byte(`{
		"object": "whatsapp_business_account",
		"entry": [{
			"changes": [{
				"field": "messages",
				"value": {
					"messages": [{
						"from": "+14155551234",
						"id": "wamid.btn",
						"type": "interactive",
						"interactive": {
							"type": "button_reply",
							"button_reply": {"id": "nomi_plan_deny:run-xyz", "title": "Deny plan"}
						}
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
	// Button replies must not create TriggerEvents.
	if fires != 0 {
		t.Fatalf("expected 0 trigger fires for button reply, got %d", fires)
	}
}

func TestSendInteractiveButtonsSuccess(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"messaging_product": "whatsapp",
			"contacts": [{"input": "+14155551234", "wa_id": "+14155551234"}],
			"messages": [{"id": "wamid.interactive"}]
		}`))
	}))
	defer srv.Close()

	prev := GraphAPIBase
	GraphAPIBase = srv.URL
	t.Cleanup(func() { GraphAPIBase = prev })

	resp, err := SendInteractiveButtons(context.Background(), srv.Client(), SendInteractiveOptions{
		PhoneNumberID: "phn-1",
		AccessToken:   "tok",
		To:            "+14155551234",
		Body:          "Plan ready",
		Buttons: []InteractiveButton{
			{ID: "nomi_plan_approve:run-1", Title: "Approve plan"},
			{ID: "nomi_plan_deny:run-1", Title: "Deny plan"},
		},
	})
	if err != nil {
		t.Fatalf("SendInteractiveButtons: %v", err)
	}
	if resp.Messages[0].ID != "wamid.interactive" {
		t.Fatalf("unexpected id: %+v", resp)
	}
	if got, _ := receivedBody["type"].(string); got != "interactive" {
		t.Fatalf("type: got %v", got)
	}
	interactive, _ := receivedBody["interactive"].(map[string]interface{})
	if interactive["type"] != "button" {
		t.Fatalf("interactive.type: %v", interactive["type"])
	}
}

func TestSendInteractiveButtonsRejectsBadCounts(t *testing.T) {
	_, err := SendInteractiveButtons(context.Background(), nil, SendInteractiveOptions{
		PhoneNumberID: "p", AccessToken: "t", To: "+1", Body: "x",
	})
	if err == nil {
		t.Fatal("expected error for zero buttons")
	}
}
