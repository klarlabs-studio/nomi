package email

import (
	"context"
	"fmt"
	"log"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins/email/transport"
)

const maxPlanStepsInMsg = 5

// planMsgRef remembers enough to reply in-thread when a plan is
// proposed or cleared. Email has no inline buttons — the user replies
// APPROVE / DENY in the thread.
type planMsgRef struct {
	ConnectionID   string
	ConversationID string
	To             string
	Subject        string
	ReplyTo        string
	References     []string
}

// subscribePlanReview listens for plan.proposed so Email threads can
// approve or deny without opening the desktop app — same gate as
// Telegram/Slack/Discord/WhatsApp (writes/patches force desktop).
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

func (p *Plugin) onPlanProposed(_ context.Context, evt *domain.Event) {
	if p.rt == nil || p.conversations == nil {
		return
	}
	run, _, plan, err := p.rt.GetRun(evt.RunID)
	if err != nil || run == nil || plan == nil || run.ConversationID == nil {
		return
	}
	conv, err := p.conversations.GetByID(*run.ConversationID)
	if err != nil || conv == nil || conv.PluginID != PluginID {
		return
	}
	conn, err := p.connections.GetByID(conv.ConnectionID)
	if err != nil || conn == nil || !conn.Enabled {
		return
	}
	cfg, err := p.buildTransportConfig(conn)
	if err != nil {
		log.Printf("[email plugin] plan prompt: transport config unavailable")
		return
	}

	to := extractFromGoal(run.Goal)
	if to == "" {
		log.Printf("[email plugin] plan prompt: no recipient in goal")
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)
	subject := planReviewSubject(run.Goal)
	replyTo := conv.ExternalConversationID
	refs := []string{}
	if replyTo != "" {
		refs = []string{replyTo}
	}

	if auto, _ := evt.Payload["auto_approved"].(bool); auto {
		if err := transport.SendEmail(cfg, []string{to}, subject,
			"Safe plan auto-approved — executing.", replyTo, refs); err != nil {
			log.Printf("[email plugin] auto-approve notice failed")
		}
		return
	}

	if requiresDesktop {
		text += "\n\nThis plan writes files or runs shell/mutating tools. Open the Nomi desktop app to review diffs — reply DENY to cancel."
	} else {
		text += "\n\nReply APPROVE to execute, or DENY to cancel."
	}

	if err := transport.SendEmail(cfg, []string{to}, subject, text, replyTo, refs); err != nil {
		log.Printf("[email plugin] post plan prompt failed: %v", err)
		return
	}

	p.mu.Lock()
	if p.planMsg == nil {
		p.planMsg = map[string]planMsgRef{}
	}
	if p.planByConv == nil {
		p.planByConv = map[string]string{}
	}
	p.planMsg[run.ID] = planMsgRef{
		ConnectionID:   conv.ConnectionID,
		ConversationID: conv.ID,
		To:             to,
		Subject:        subject,
		ReplyTo:        replyTo,
		References:     refs,
	}
	p.planByConv[conv.ID] = run.ID
	p.mu.Unlock()
}

func (p *Plugin) clearPlanPrompt(_ context.Context, runID, label string) {
	if runID == "" {
		return
	}
	p.mu.Lock()
	ref, ok := p.planMsg[runID]
	delete(p.planMsg, runID)
	if ref.ConversationID != "" {
		delete(p.planByConv, ref.ConversationID)
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	conn, err := p.connections.GetByID(ref.ConnectionID)
	if err != nil || conn == nil {
		return
	}
	cfg, err := p.buildTransportConfig(conn)
	if err != nil {
		return
	}
	_ = transport.SendEmail(cfg, []string{ref.To}, ref.Subject, label, ref.ReplyTo, ref.References)
}

func formatPlanReviewText(goal string, plan *domain.Plan) string {
	var b strings.Builder
	b.WriteString("Plan ready for review\n")
	if goal != "" {
		b.WriteString("Goal: ")
		b.WriteString(truncateRunes(stripFromPrefix(goal), 200))
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
	return b.String()
}

func planRequiresDesktopReview(plan *domain.Plan) bool {
	return domain.PlanRequiresDesktopReview(plan)
}

func planReviewSubject(goal string) string {
	first := strings.TrimSpace(strings.Split(goal, "\n")[0])
	if first == "" || first == "(no subject)" {
		return "Re: Nomi plan review"
	}
	if strings.HasPrefix(strings.ToLower(first), "re:") {
		return first
	}
	return "Re: " + truncateRunes(first, 80)
}

// extractFromGoal pulls the "From: addr" line injected by handleMessage.
func extractFromGoal(goal string) string {
	for _, line := range strings.Split(goal, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "from: ") {
			addr := strings.TrimSpace(line[6:])
			if parsed, _ := transport.ParseAddress(addr); parsed != "" {
				return parsed
			}
			return addr
		}
	}
	return ""
}

func stripFromPrefix(goal string) string {
	// Prefer the subject line (first line) for the Goal: summary.
	first := strings.TrimSpace(strings.Split(goal, "\n")[0])
	if first != "" {
		return first
	}
	return goal
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// parsePlanReply returns (approve, true) when the body is a bare
// APPROVE/DENY (optionally with surrounding whitespace). Anything else
// is treated as a normal new goal so users can keep chatting.
func parsePlanReply(body string) (approve bool, ok bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return false, false
	}
	// Use first non-empty line so quoted reply chains still match when
	// the user typed APPROVE at the top.
	first := trimmed
	if i := strings.IndexAny(trimmed, "\r\n"); i >= 0 {
		first = strings.TrimSpace(trimmed[:i])
	}
	switch strings.ToUpper(first) {
	case "APPROVE", "APPROVE PLAN", "YES":
		return true, true
	case "DENY", "DENY PLAN", "NO", "CANCEL":
		return false, true
	default:
		return false, false
	}
}

// tryHandlePlanReply short-circuits inbound mail when the thread has a
// pending plan_review run and the body is APPROVE/DENY. Returns true
// when the message was consumed (do not CreateRun).
func (p *Plugin) tryHandlePlanReply(ctx context.Context, connID string, cfg transport.Config, m transport.Message, conversationID, senderAddr string) bool {
	if conversationID == "" || p.rt == nil {
		return false
	}
	approve, isReply := parsePlanReply(m.Body)
	if !isReply {
		return false
	}

	p.mu.RLock()
	runID, ok := p.planByConv[conversationID]
	p.mu.RUnlock()
	if !ok || runID == "" {
		// Fall back: scan recent runs for plan_review on this conversation.
		runID = p.findPendingPlanRun(conversationID)
		if runID == "" {
			return false
		}
	}

	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		return false
	}
	if p.identities != nil && senderAddr != "" {
		existing, err := p.identities.ListByConnection(connID)
		if err == nil && len(existing) > 0 {
			allowed, _ := p.identities.IsAllowed(PluginID, connID, senderAddr, assistantID)
			if !allowed {
				log.Printf("[email plugin] plan reply from non-allowlisted sender")
				return true // consumed but rejected
			}
		}
	}

	subject := planReviewSubject(m.Subject)
	replyTo := m.MessageID
	if replyTo == "" {
		replyTo = resolveThreadKey(m)
	}
	refs := m.References
	if replyTo != "" && len(refs) == 0 {
		refs = []string{replyTo}
	}

	if approve {
		run, _, plan, err := p.rt.GetRun(runID)
		if err != nil || run == nil {
			return true
		}
		if plan != nil && planRequiresDesktopReview(plan) {
			_ = transport.SendEmail(cfg, []string{senderAddr}, subject,
				"This plan needs DiffPreview in the Nomi desktop app — reply DENY to cancel, or approve there.",
				replyTo, refs)
			return true
		}
		if err := p.rt.ApprovePlan(ctx, runID); err != nil {
			log.Printf("[email plugin] ApprovePlan failed: %v", err)
			_ = transport.SendEmail(cfg, []string{senderAddr}, subject,
				"Could not approve plan (it may already be resolved).", replyTo, refs)
			return true
		}
		_ = transport.SendEmail(cfg, []string{senderAddr}, subject,
			"Plan approved — executing.", replyTo, refs)
	} else {
		if err := p.rt.CancelRun(ctx, runID); err != nil {
			log.Printf("[email plugin] CancelRun failed: %v", err)
			_ = transport.SendEmail(cfg, []string{senderAddr}, subject,
				"Could not deny plan (it may already be resolved).", replyTo, refs)
			return true
		}
		_ = transport.SendEmail(cfg, []string{senderAddr}, subject,
			"Plan denied / run cancelled.", replyTo, refs)
	}

	p.mu.Lock()
	delete(p.planMsg, runID)
	delete(p.planByConv, conversationID)
	p.mu.Unlock()
	return true
}

func (p *Plugin) findPendingPlanRun(conversationID string) string {
	if p.rt == nil {
		return ""
	}
	runs, err := p.rt.ListRuns()
	if err != nil {
		return ""
	}
	for _, r := range runs {
		if r == nil || r.ConversationID == nil {
			continue
		}
		if *r.ConversationID == conversationID && r.Status == domain.RunPlanReview {
			return r.ID
		}
	}
	return ""
}
