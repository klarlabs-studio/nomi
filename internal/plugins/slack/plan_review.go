package slack

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/slack-go/slack"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

const (
	actionPlanApprove = "nomi_plan_approve:"
	actionPlanDeny    = "nomi_plan_deny:"
	maxPlanStepsInMsg = 5
)

// subscribePlanReview listens for plan.proposed (and clear signals) so
// Slack threads can approve or deny a plan without opening the desktop
// app — mirroring the existing tool-approval Block Kit UX.
func (p *Plugin) subscribePlanReview(ctx context.Context) {
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
	if p.rt == nil || p.runs == nil {
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
	channel, threadTS, err := splitExternalID(conv.ExternalConversationID)
	if err != nil {
		return
	}
	p.mu.RLock()
	client, ok := p.clients[conv.ConnectionID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)
	blocks := buildPlanReviewBlocks(text, run.ID, requiresDesktop)

	edited, _ := evt.Payload["edited"].(bool)
	p.mu.Lock()
	ref, haveRef := p.planMsgTS[run.ID]
	p.mu.Unlock()
	if edited && haveRef {
		_, _, _, err := client.UpdateMessageContext(
			ctx, ref.Channel, ref.TS,
			slack.MsgOptionText(text, false),
			slack.MsgOptionBlocks(blocks...),
		)
		if err != nil {
			log.Printf("[slack plugin] edit plan prompt: %v", err)
		}
		return
	}

	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	postedChannel, postedTS, err := client.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		log.Printf("[slack plugin] post plan block: %v", err)
		return
	}
	p.mu.Lock()
	p.planMsgTS[run.ID] = approvalMsgRef{
		ConnectionID: conv.ConnectionID,
		Channel:      postedChannel,
		TS:           postedTS,
	}
	p.mu.Unlock()
}

func (p *Plugin) clearPlanPrompt(ctx context.Context, runID, label string) {
	if runID == "" {
		return
	}
	p.mu.Lock()
	ref, ok := p.planMsgTS[runID]
	delete(p.planMsgTS, runID)
	p.mu.Unlock()
	if !ok {
		return
	}
	p.mu.RLock()
	client, ok := p.clients[ref.ConnectionID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	_, _, _, err := client.UpdateMessageContext(ctx, ref.Channel, ref.TS, slack.MsgOptionText(label, false))
	if err != nil {
		log.Printf("[slack plugin] clear plan prompt: %v", err)
	}
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
			fmt.Fprintf(&b, " — `%s`", cap)
		}
		b.WriteString("\n")
	}
	if len(steps) > maxPlanStepsInMsg {
		fmt.Fprintf(&b, "(+%d more)\n", len(steps)-maxPlanStepsInMsg)
	}
	if planRequiresDesktopReview(plan) {
		b.WriteString("\n_This plan writes files or runs shell/mutating tools. Approve in the Nomi desktop app to review diffs._")
	} else {
		b.WriteString("\nApprove to execute. Deny cancels the run.")
	}
	return b.String()
}

func planRequiresDesktopReview(plan *domain.Plan) bool {
	if plan == nil {
		return false
	}
	for _, s := range plan.Steps {
		cap := s.ExpectedCapability
		tool := s.ExpectedTool
		if cap == "filesystem.write" || tool == "filesystem.write" || tool == "filesystem.patch" {
			return true
		}
		if (cap == "command.exec" || tool == "command.exec") && isIrreversibleCommand(stepCommand(s)) {
			return true
		}
		name := tool
		if name == "" {
			name = cap
		}
		if (strings.HasPrefix(cap, "mcp.") || strings.HasPrefix(tool, "mcp.")) && isMutatingToolName(name) {
			return true
		}
	}
	return false
}

func stepCommand(s domain.StepDefinition) string {
	if s.Arguments == nil {
		return ""
	}
	if cmd, ok := s.Arguments["command"].(string); ok {
		return cmd
	}
	if cmd, ok := s.Arguments["input"].(string); ok {
		return cmd
	}
	return ""
}

func isIrreversibleCommand(cmd string) bool {
	lower := strings.ToLower(cmd)
	return strings.Contains(lower, "rm -rf") ||
		strings.HasPrefix(lower, "rm ") ||
		strings.Contains(lower, "mkfs") ||
		strings.Contains(lower, "dd if=")
}

func isMutatingToolName(name string) bool {
	n := strings.ToLower(name)
	for _, needle := range []string{
		"write", "delete", "remove", "create", "update", "patch",
		"put", "send", "post", "exec", "run", "destroy", "drop",
		"insert", "mutate",
	} {
		if strings.Contains(n, needle) {
			return true
		}
	}
	return false
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// buildPlanReviewBlocks builds Block Kit for plan approve/deny. When
// requiresDesktop is true, only Deny is offered so write/patch plans
// still go through DiffPreview in the desktop app.
func buildPlanReviewBlocks(text, runID string, requiresDesktop bool) []slack.Block {
	var actions []slack.BlockElement
	if !requiresDesktop {
		approveBtn := slack.NewButtonBlockElement(
			actionPlanApprove+runID,
			runID,
			slack.NewTextBlockObject(slack.PlainTextType, "Approve plan", false, false),
		)
		approveBtn.Style = slack.StylePrimary
		actions = append(actions, approveBtn)
	}
	denyBtn := slack.NewButtonBlockElement(
		actionPlanDeny+runID,
		runID,
		slack.NewTextBlockObject(slack.PlainTextType, "Deny plan", false, false),
	)
	denyBtn.Style = slack.StyleDanger
	actions = append(actions, denyBtn)

	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil, nil,
		),
		slack.NewActionBlock("nomi_plan_actions", actions...),
	}
}
