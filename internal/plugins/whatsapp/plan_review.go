package whatsapp

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/secrets"
)

const (
	callbackPlanApprove = "nomi_plan_approve:"
	callbackPlanDeny    = "nomi_plan_deny:"
	maxPlanStepsInMsg   = 5
	// WhatsApp reply-button titles are capped at 20 characters.
	btnApproveTitle = "Approve plan"
	btnDenyTitle    = "Deny plan"
)

type planMsgRef struct {
	ConnectionID string
	To           string // E.164 / wa_id
}

// subscribePlanReview listens for plan.proposed so WhatsApp chats can
// approve or deny a plan without opening the desktop app — same
// semantics as Telegram/Slack/Discord.
func (p *Plugin) subscribePlanReview(ctx context.Context) {
	if p.eventBus == nil {
		return
	}
	sub := p.eventBus.Subscribe(events.EventFilter{
		EventTypes: []domain.EventType{
			domain.EventPlanProposed,
			domain.EventStepStarted,
			domain.EventRunCancelled,
			domain.EventRunFailed,
			domain.EventRunCompleted,
		},
	})
	defer sub.Unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sub.Events():
			if !ok {
				return
			}
			switch evt.Type {
			case domain.EventPlanProposed:
				p.onPlanProposed(ctx, evt)
			case domain.EventStepStarted, domain.EventRunCancelled, domain.EventRunFailed, domain.EventRunCompleted:
				p.clearPlanPrompt(ctx, evt.RunID, planClearLabel(evt.Type))
			}
		}
	}
}

func planClearLabel(t domain.EventType) string {
	switch t {
	case domain.EventStepStarted:
		return "Plan approved — executing."
	case domain.EventRunCancelled:
		return "Plan denied / run cancelled."
	case domain.EventRunFailed:
		return "Run failed."
	case domain.EventRunCompleted:
		return "Run completed."
	default:
		return "Resolved."
	}
}

func (p *Plugin) onPlanProposed(ctx context.Context, evt *domain.Event) {
	if p.rt == nil || p.runs == nil || p.conversations == nil {
		return
	}
	run, err := p.runs.GetByID(evt.RunID)
	if err != nil || run == nil || run.ConversationID == nil {
		return
	}
	conv, err := p.conversations.GetByID(*run.ConversationID)
	if err != nil || conv == nil || conv.PluginID != PluginID {
		return
	}
	_, _, plan, err := p.rt.GetRun(evt.RunID)
	if err != nil || plan == nil {
		return
	}
	conn, err := p.connections.GetByID(conv.ConnectionID)
	if err != nil || conn == nil || !conn.Enabled {
		return
	}
	creds, err := p.resolveSendCreds(conn)
	if err != nil {
		log.Printf("[whatsapp plugin] plan prompt creds unavailable")
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)
	client := &http.Client{Timeout: 15 * time.Second}
	if auto, _ := evt.Payload["auto_approved"].(bool); auto {
		_, err = SendText(ctx, client, SendTextOptions{
			PhoneNumberID: creds.phoneNumberID,
			AccessToken:   creds.accessToken,
			To:            conv.ExternalConversationID,
			Body:          "Safe plan auto-approved — executing.",
		})
		if err != nil {
			log.Printf("[whatsapp plugin] auto-approve notice failed")
		}
		return
	}

	_, err = SendInteractiveButtons(ctx, client, SendInteractiveOptions{
		PhoneNumberID: creds.phoneNumberID,
		AccessToken:   creds.accessToken,
		To:            conv.ExternalConversationID,
		Body:          text,
		Buttons:       planReviewButtons(run.ID, requiresDesktop),
	})
	if err != nil {
		log.Printf("[whatsapp plugin] post plan prompt failed")
		return
	}
	p.mu.Lock()
	p.planMsg[run.ID] = planMsgRef{
		ConnectionID: conv.ConnectionID,
		To:           conv.ExternalConversationID,
	}
	p.mu.Unlock()
}

func (p *Plugin) clearPlanPrompt(ctx context.Context, runID, label string) {
	if runID == "" {
		return
	}
	p.mu.Lock()
	ref, ok := p.planMsg[runID]
	delete(p.planMsg, runID)
	p.mu.Unlock()
	if !ok {
		return
	}
	conn, err := p.connections.GetByID(ref.ConnectionID)
	if err != nil || conn == nil {
		return
	}
	creds, err := p.resolveSendCreds(conn)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 15 * time.Second}
	_, _ = SendText(ctx, client, SendTextOptions{
		PhoneNumberID: creds.phoneNumberID,
		AccessToken:   creds.accessToken,
		To:            ref.To,
		Body:          label,
	})
}

type sendCreds struct {
	phoneNumberID string
	accessToken   string
}

func (p *Plugin) resolveSendCreds(conn *domain.Connection) (sendCreds, error) {
	phoneNumberID, _ := conn.Config["phone_number_id"].(string)
	if phoneNumberID == "" {
		return sendCreds{}, fmt.Errorf("connection %s missing phone_number_id", conn.ID)
	}
	tokenRef, ok := conn.CredentialRefs["access_token"]
	if !ok || tokenRef == "" {
		return sendCreds{}, fmt.Errorf("connection %s missing access_token", conn.ID)
	}
	token := tokenRef
	if p.secrets != nil {
		resolved, err := secrets.Resolve(p.secrets, tokenRef)
		if err != nil {
			return sendCreds{}, err
		}
		token = resolved
	}
	return sendCreds{phoneNumberID: phoneNumberID, accessToken: token}, nil
}

func formatPlanReviewText(goal string, plan *domain.Plan) string {
	var b strings.Builder
	b.WriteString("*Plan ready for review*\n")
	if goal != "" {
		b.WriteString("Goal: ")
		b.WriteString(truncateRunes(goal, 200))
		b.WriteString("\n\n")
	}
	steps := plan.Steps
	limit := maxPlanStepsInMsg
	if len(steps) < limit {
		limit = len(steps)
	}
	for i := 0; i < limit; i++ {
		s := steps[i]
		title := s.Title
		if title == "" {
			title = s.ExpectedTool
		}
		if title == "" {
			title = "step"
		}
		fmt.Fprintf(&b, "%d. %s", i+1, truncateRunes(title, 80))
		cap := s.ExpectedCapability
		if cap == "" {
			cap = s.ExpectedTool
		}
		if cap != "" {
			fmt.Fprintf(&b, " — %s", cap)
		}
		b.WriteString("\n")
	}
	if len(steps) > maxPlanStepsInMsg {
		fmt.Fprintf(&b, "(+%d more)\n", len(steps)-maxPlanStepsInMsg)
	}
	if planRequiresDesktopReview(plan) {
		b.WriteString("\nThis plan writes files or runs shell/mutating tools. Approve in the Nomi desktop app to review diffs.")
	} else {
		b.WriteString("\nApprove to execute. Deny cancels the run.")
	}
	return b.String()
}

func planReviewButtons(runID string, requiresDesktop bool) []InteractiveButton {
	var buttons []InteractiveButton
	if !requiresDesktop {
		buttons = append(buttons, InteractiveButton{
			ID:    callbackPlanApprove + runID,
			Title: btnApproveTitle,
		})
	}
	buttons = append(buttons, InteractiveButton{
		ID:    callbackPlanDeny + runID,
		Title: btnDenyTitle,
	})
	return buttons
}

func planRequiresDesktopReview(plan *domain.Plan) bool {
	return domain.PlanRequiresDesktopReview(plan)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// handlePlanButtonReply resolves Approve/Deny interactive button presses.
func (p *Plugin) handlePlanButtonReply(ctx context.Context, connID, from, buttonID string) {
	if buttonID == "" || p.rt == nil {
		return
	}
	var runID string
	var approve bool
	switch {
	case strings.HasPrefix(buttonID, callbackPlanApprove):
		runID = strings.TrimPrefix(buttonID, callbackPlanApprove)
		approve = true
	case strings.HasPrefix(buttonID, callbackPlanDeny):
		runID = strings.TrimPrefix(buttonID, callbackPlanDeny)
		approve = false
	default:
		return
	}
	if runID == "" {
		return
	}

	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, from) {
		log.Printf("[whatsapp plugin] plan button from non-allowlisted sender")
		return
	}

	conn, err := p.connections.GetByID(connID)
	if err != nil || conn == nil {
		return
	}
	creds, err := p.resolveSendCreds(conn)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 15 * time.Second}

	if approve {
		if err := p.rt.ApprovePlan(ctx, runID); err != nil {
			log.Printf("[whatsapp plugin] ApprovePlan failed")
			_, _ = SendText(ctx, client, SendTextOptions{
				PhoneNumberID: creds.phoneNumberID,
				AccessToken:   creds.accessToken,
				To:            from,
				Body:          "Could not approve plan (it may already be resolved).",
			})
			return
		}
		_, _ = SendText(ctx, client, SendTextOptions{
			PhoneNumberID: creds.phoneNumberID,
			AccessToken:   creds.accessToken,
			To:            from,
			Body:          "Plan approved — executing.",
		})
	} else {
		if err := p.rt.CancelRun(ctx, runID); err != nil {
			log.Printf("[whatsapp plugin] CancelRun failed")
			_, _ = SendText(ctx, client, SendTextOptions{
				PhoneNumberID: creds.phoneNumberID,
				AccessToken:   creds.accessToken,
				To:            from,
				Body:          "Could not deny plan (it may already be resolved).",
			})
			return
		}
		_, _ = SendText(ctx, client, SendTextOptions{
			PhoneNumberID: creds.phoneNumberID,
			AccessToken:   creds.accessToken,
			To:            from,
			Body:          "Plan denied / run cancelled.",
		})
	}
	p.mu.Lock()
	delete(p.planMsg, runID)
	p.mu.Unlock()
}
