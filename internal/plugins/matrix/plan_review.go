package matrix

import (
	"context"
	"fmt"
	"log"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

const maxPlanStepsInMsg = 5

// planMsgRef remembers the prompt event so reactions and room replies
// can resolve Approve/Deny without opening the desktop app.
type planMsgRef struct {
	ConnectionID string
	RoomID       string
	EventID      string
	RequiresDesktop bool
}

// subscribePlanReview listens for plan.proposed so Matrix rooms can
// approve or deny — same gate as Email/Discord (writes force desktop).
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
		if _, err := cli.sendText(ctx, conv.ExternalConversationID, "Safe plan auto-approved — executing."); err != nil {
			log.Printf("[matrix plugin] auto-approve notice failed")
		}
		return
	}

	edited, _ := evt.Payload["edited"].(bool)
	p.mu.Lock()
	ref, haveRef := p.planMsg[run.ID]
	p.mu.Unlock()
	if edited && haveRef {
		// Matrix has no reliable edit-in-place across all clients for
		// bot prompts; post a fresh prompt and replace the ref.
		_ = ref
	}

	eventID, err := cli.sendText(ctx, conv.ExternalConversationID, text)
	if err != nil {
		log.Printf("[matrix plugin] post plan prompt failed: %v", err)
		return
	}
	p.mu.Lock()
	p.planMsg[run.ID] = planMsgRef{
		ConnectionID:    conv.ConnectionID,
		RoomID:          conv.ExternalConversationID,
		EventID:         eventID,
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
	if _, err := cli.sendText(ctx, ref.RoomID, label); err != nil {
		log.Printf("[matrix plugin] clear plan prompt failed")
	}
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
			fmt.Fprintf(&b, " — `%s`", cap)
		}
		b.WriteString("\n")
	}
	if len(steps) > maxPlanStepsInMsg {
		fmt.Fprintf(&b, "(+%d more)\n", len(steps)-maxPlanStepsInMsg)
	}
	if planRequiresDesktopReview(plan) {
		b.WriteString("\n_This plan writes files or runs shell/mutating tools. Approve in the Nomi desktop app to review diffs._\n")
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

// parsePlanReply mirrors email: bare APPROVE/DENY (first line).
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

// parseReactionKey maps common Matrix reaction keys to approve/deny.
func parseReactionKey(key string) (approve bool, ok bool) {
	switch strings.TrimSpace(key) {
	case "✅", "☑️", "✔", "👍", "+1", "white_check_mark":
		return true, true
	case "❌", "❎", "👎", "-1", "x", "X":
		return false, true
	}
	return false, false
}

// tryHandlePlanReply resolves APPROVE/DENY text for the latest pending
// plan in this room. Returns true when the message was consumed.
func (p *Plugin) tryHandlePlanReply(ctx context.Context, connID, roomID, sender, body string) bool {
	approve, isReply := parsePlanReply(body)
	if !isReply || p.rt == nil {
		return false
	}
	runID, requiresDesktop, ok := p.findPlanForRoom(connID, roomID)
	if !ok {
		return false
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, sender) {
		return true // consume but ignore unauthorized
	}
	return p.resolvePlan(ctx, runID, approve, requiresDesktop)
}

func (p *Plugin) handleReaction(ctx context.Context, connID, roomID string, ev rawEvent) {
	targetID, key, ok := parseReaction(ev.Content)
	if !ok {
		return
	}
	approve, ok := parseReactionKey(key)
	if !ok || p.rt == nil {
		return
	}
	runID, requiresDesktop, ok := p.findPlanByEvent(connID, roomID, targetID)
	if !ok {
		return
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, ev.Sender) {
		return
	}
	_ = p.resolvePlan(ctx, runID, approve, requiresDesktop)
}

func (p *Plugin) findPlanForRoom(connID, roomID string) (runID string, requiresDesktop bool, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Prefer the most recently tracked prompt in this room (map iteration
	// order is random — pick any match; typically one pending plan).
	for id, ref := range p.planMsg {
		if ref.ConnectionID == connID && ref.RoomID == roomID {
			return id, ref.RequiresDesktop, true
		}
	}
	return "", false, false
}

func (p *Plugin) findPlanByEvent(connID, roomID, eventID string) (runID string, requiresDesktop bool, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, ref := range p.planMsg {
		if ref.ConnectionID == connID && ref.RoomID == roomID && ref.EventID == eventID {
			return id, ref.RequiresDesktop, true
		}
	}
	return "", false, false
}

func (p *Plugin) resolvePlan(ctx context.Context, runID string, approve, requiresDesktop bool) bool {
	if approve && requiresDesktop {
		p.mu.RLock()
		ref := p.planMsg[runID]
		cli := p.clients[ref.ConnectionID]
		p.mu.RUnlock()
		if cli != nil && ref.RoomID != "" {
			_, _ = cli.sendText(ctx, ref.RoomID,
				"This plan needs DiffPreview in the Nomi desktop app — Approve there, or reply DENY to cancel.")
		}
		return true
	}
	if approve {
		if err := p.rt.ApprovePlan(ctx, runID); err != nil {
			log.Printf("[matrix plugin] ApprovePlan failed: %v", err)
			return true
		}
	} else {
		if err := p.rt.CancelRun(ctx, runID); err != nil {
			log.Printf("[matrix plugin] CancelRun failed: %v", err)
			return true
		}
	}
	p.mu.Lock()
	delete(p.planMsg, runID)
	p.mu.Unlock()
	return true
}
