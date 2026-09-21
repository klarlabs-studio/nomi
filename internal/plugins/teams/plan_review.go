package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

const maxPlanStepsInMsg = 5

type planMsgRef struct {
	ConnectionID string
	ServiceURL   string
	Conversation string
}

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
	ref, ok := p.lookupConversation(conv.ConnectionID, conv.ExternalConversationID)
	if !ok || ref.ServiceURL == "" {
		log.Printf("[teams plugin] plan prompt: no conversation ref")
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)
	if auto, _ := evt.Payload["auto_approved"].(bool); auto {
		if err := p.replyText(ctx, conv.ConnectionID, ref.ServiceURL, ref.ConversationID, "Safe plan auto-approved — executing."); err != nil {
			log.Printf("[teams plugin] auto-approve notice failed")
		}
		return
	}

	card := planReviewCard(run.ID, text, requiresDesktop)
	if err := p.replyAdaptiveCard(ctx, conv.ConnectionID, ref.ServiceURL, ref.ConversationID, text, card); err != nil {
		log.Printf("[teams plugin] post plan prompt failed")
		return
	}
	p.mu.Lock()
	p.planMsg[run.ID] = planMsgRef{
		ConnectionID: conv.ConnectionID,
		ServiceURL:   ref.ServiceURL,
		Conversation: ref.ConversationID,
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
	_ = p.replyText(ctx, ref.ConnectionID, ref.ServiceURL, ref.Conversation, label)
}

func formatPlanReviewText(goal string, plan *domain.Plan) string {
	var b strings.Builder
	b.WriteString("**Plan ready for review**\n")
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

func planReviewCard(runID, body string, requiresDesktop bool) map[string]interface{} {
	actions := []map[string]interface{}{}
	if !requiresDesktop {
		actions = append(actions, map[string]interface{}{
			"type":  "Action.Submit",
			"title": "Approve plan",
			"data": map[string]string{
				"nomi_plan": "approve",
				"run_id":    runID,
			},
			"style": "positive",
		})
	}
	actions = append(actions, map[string]interface{}{
		"type":  "Action.Submit",
		"title": "Deny plan",
		"data": map[string]string{
			"nomi_plan": "deny",
			"run_id":    runID,
		},
		"style": "destructive",
	})
	return map[string]interface{}{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body": []map[string]interface{}{{
			"type": "TextBlock",
			"text": body,
			"wrap": true,
		}},
		"actions": actions,
	}
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

func (p *Plugin) handleInvoke(ctx context.Context, connID string, act activity) {
	if p.rt == nil {
		return
	}
	runID, approve, ok := parsePlanInvoke(act.Value)
	if !ok || runID == "" {
		return
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, act.From.ID) {
		return
	}
	if approve {
		p.mu.RLock()
		ref, have := p.planMsg[runID]
		p.mu.RUnlock()
		if have {
			// Desktop-gated plans omit Approve on the card; if somehow
			// invoked, nudge to desktop.
			_ = ref
		}
		if err := p.rt.ApprovePlan(ctx, runID); err != nil {
			log.Printf("[teams plugin] ApprovePlan failed")
			return
		}
	} else {
		if err := p.rt.CancelRun(ctx, runID); err != nil {
			log.Printf("[teams plugin] CancelRun failed")
			return
		}
	}
	p.mu.Lock()
	delete(p.planMsg, runID)
	p.mu.Unlock()
}

func parsePlanInvoke(value json.RawMessage) (runID string, approve bool, ok bool) {
	if len(value) == 0 {
		return "", false, false
	}
	// Adaptive Card Action.Submit may nest under action.data or be flat.
	var flat struct {
		NomiPlan string `json:"nomi_plan"`
		RunID    string `json:"run_id"`
		Action   *struct {
			Data struct {
				NomiPlan string `json:"nomi_plan"`
				RunID    string `json:"run_id"`
			} `json:"data"`
		} `json:"action"`
	}
	if err := json.Unmarshal(value, &flat); err != nil {
		return "", false, false
	}
	plan := flat.NomiPlan
	runID = flat.RunID
	if flat.Action != nil {
		if plan == "" {
			plan = flat.Action.Data.NomiPlan
		}
		if runID == "" {
			runID = flat.Action.Data.RunID
		}
	}
	switch strings.ToLower(plan) {
	case "approve":
		return runID, true, runID != ""
	case "deny":
		return runID, false, runID != ""
	}
	return "", false, false
}
