package signal

import (
	"context"
	"fmt"
	"log"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

const maxPlanStepsInMsg = 5

type planMsgRef struct {
	ConnectionID    string
	Recipient       string // E.164 or group:<id>
	RequiresDesktop bool
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
	p.mu.RLock()
	cli := p.clients[conv.ConnectionID]
	p.mu.RUnlock()
	if cli == nil {
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)
	if auto, _ := evt.Payload["auto_approved"].(bool); auto {
		if err := cli.sendText(ctx, conv.ExternalConversationID, "Safe plan auto-approved — executing."); err != nil {
			log.Printf("[signal plugin] auto-approve notice failed")
		}
		return
	}
	if err := cli.sendText(ctx, conv.ExternalConversationID, text); err != nil {
		log.Printf("[signal plugin] post plan prompt failed")
		return
	}
	p.mu.Lock()
	p.planMsg[run.ID] = planMsgRef{
		ConnectionID:    conv.ConnectionID,
		Recipient:       conv.ExternalConversationID,
		RequiresDesktop: requiresDesktop,
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
	cli := p.clients[ref.ConnectionID]
	p.mu.Unlock()
	if !ok || cli == nil {
		return
	}
	_ = cli.sendText(ctx, ref.Recipient, label)
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
		b.WriteString("\nThis plan writes files or runs shell/mutating tools. Approve in the Nomi desktop app to review diffs.\n")
		b.WriteString("Reply DENY (or react ❌) to cancel.")
	} else {
		b.WriteString("\nReply APPROVE to execute, or DENY to cancel.\n")
		b.WriteString("Or react ✅ / ❌ on this message.")
	}
	return b.String()
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

func parsePlanReply(body string) (approve bool, ok bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return false, false
	}
	first := trimmed
	if i := strings.IndexAny(trimmed, "\r\n"); i >= 0 {
		first = strings.TrimSpace(trimmed[:i])
	}
	switch strings.ToUpper(first) {
	case "APPROVE", "APPROVE PLAN", "YES":
		return true, true
	case "DENY", "DENY PLAN", "NO", "CANCEL":
		return false, true
	}
	return false, false
}

func parseReactionKey(key string) (approve bool, ok bool) {
	switch strings.TrimSpace(key) {
	case "✅", "☑️", "✔", "👍", "+1":
		return true, true
	case "❌", "❎", "👎", "-1", "x", "X":
		return false, true
	}
	return false, false
}

func (p *Plugin) tryHandlePlanReply(ctx context.Context, connID, sender, convKey, body string) bool {
	approve, isReply := parsePlanReply(body)
	if !isReply || p.rt == nil {
		return false
	}
	runID, requiresDesktop, ok := p.findPlanForConversation(connID, convKey)
	if !ok {
		return false
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, sender) {
		return true
	}
	return p.resolvePlan(ctx, runID, approve, requiresDesktop, connID, convKey)
}

func (p *Plugin) handleReaction(ctx context.Context, connID, sender, convKey, emoji string) {
	approve, ok := parseReactionKey(emoji)
	if !ok || p.rt == nil {
		return
	}
	runID, requiresDesktop, ok := p.findPlanForConversation(connID, convKey)
	if !ok {
		return
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, sender) {
		return
	}
	_ = p.resolvePlan(ctx, runID, approve, requiresDesktop, connID, convKey)
}

func (p *Plugin) findPlanForConversation(connID, convKey string) (runID string, requiresDesktop bool, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, ref := range p.planMsg {
		if ref.ConnectionID == connID && ref.Recipient == convKey {
			return id, ref.RequiresDesktop, true
		}
	}
	return "", false, false
}

func (p *Plugin) resolvePlan(ctx context.Context, runID string, approve, requiresDesktop bool, connID, convKey string) bool {
	if approve && requiresDesktop {
		p.mu.RLock()
		cli := p.clients[connID]
		p.mu.RUnlock()
		if cli != nil {
			_ = cli.sendText(ctx, convKey,
				"This plan needs DiffPreview in the Nomi desktop app — Approve there, or reply DENY to cancel.")
		}
		return true
	}
	if approve {
		if err := p.rt.ApprovePlan(ctx, runID); err != nil {
			log.Printf("[signal plugin] ApprovePlan failed")
			return true
		}
	} else {
		if err := p.rt.CancelRun(ctx, runID); err != nil {
			log.Printf("[signal plugin] CancelRun failed")
			return true
		}
	}
	p.mu.Lock()
	delete(p.planMsg, runID)
	p.mu.Unlock()
	return true
}
