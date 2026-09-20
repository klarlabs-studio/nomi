package discord

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

const (
	callbackPlanApprove = "nomi_plan_approve:"
	callbackPlanDeny    = "nomi_plan_deny:"
	maxPlanStepsInMsg   = 5
)

type planMsgRef struct {
	ConnectionID string
	ChannelID    string
	MessageID    string
}

// subscribePlanReview listens for plan.proposed so Discord chats can
// approve or deny a plan without opening the desktop app — same
// semantics as Telegram/Slack.
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
				p.clearPlanPrompt(evt.RunID, planClearLabel(evt.Type))
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
	sess := p.sessions[conv.ConnectionID]
	p.mu.RUnlock()
	if sess == nil {
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)
	components := planReviewComponents(run.ID, requiresDesktop)

	edited, _ := evt.Payload["edited"].(bool)
	p.mu.Lock()
	ref, haveRef := p.planMsg[run.ID]
	p.mu.Unlock()
	if edited && haveRef {
		_, err := sess.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel:    ref.ChannelID,
			ID:         ref.MessageID,
			Content:    &text,
			Components: &components,
		})
		if err != nil {
			log.Printf("[discord plugin] edit plan prompt failed")
		}
		return
	}

	msg, err := sess.ChannelMessageSendComplex(conv.ExternalConversationID, &discordgo.MessageSend{
		Content:    text,
		Components: components,
	})
	if err != nil {
		log.Printf("[discord plugin] post plan prompt failed")
		return
	}
	p.mu.Lock()
	p.planMsg[run.ID] = planMsgRef{
		ConnectionID: conv.ConnectionID,
		ChannelID:    conv.ExternalConversationID,
		MessageID:    msg.ID,
	}
	p.mu.Unlock()
	_ = ctx
}

func (p *Plugin) clearPlanPrompt(runID, label string) {
	if runID == "" {
		return
	}
	p.mu.Lock()
	ref, ok := p.planMsg[runID]
	delete(p.planMsg, runID)
	sess := p.sessions[ref.ConnectionID]
	p.mu.Unlock()
	if !ok || sess == nil {
		return
	}
	empty := []discordgo.MessageComponent{}
	_, _ = sess.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    ref.ChannelID,
		ID:         ref.MessageID,
		Content:    &label,
		Components: &empty,
	})
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
		b.WriteString("\n_This plan writes files or runs shell/mutating tools. Approve in the Nomi desktop app to review diffs._")
	} else {
		b.WriteString("\nApprove to execute. Deny cancels the run.")
	}
	return b.String()
}

func planReviewComponents(runID string, requiresDesktop bool) []discordgo.MessageComponent {
	row := []discordgo.MessageComponent{}
	if !requiresDesktop {
		row = append(row, discordgo.Button{
			Label:    "Approve plan",
			Style:    discordgo.SuccessButton,
			CustomID: callbackPlanApprove + runID,
		})
	}
	row = append(row, discordgo.Button{
		Label:    "Deny plan",
		Style:    discordgo.DangerButton,
		CustomID: callbackPlanDeny + runID,
	})
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: row},
	}
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

// handlePlanInteraction resolves Approve/Deny button presses.
func (p *Plugin) handlePlanInteraction(ctx context.Context, connID string, s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent || i.MessageComponentData().CustomID == "" {
		return
	}
	customID := i.MessageComponentData().CustomID
	var runID string
	var approve bool
	switch {
	case strings.HasPrefix(customID, callbackPlanApprove):
		runID = strings.TrimPrefix(customID, callbackPlanApprove)
		approve = true
	case strings.HasPrefix(customID, callbackPlanDeny):
		runID = strings.TrimPrefix(customID, callbackPlanDeny)
		approve = false
	default:
		return
	}
	if runID == "" || p.rt == nil {
		return
	}

	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil || !p.senderAllowed(connID, assistantID, userID) {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You are not allowlisted for this assistant.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if approve {
		if err := p.rt.ApprovePlan(ctx, runID); err != nil {
			log.Printf("[discord plugin] ApprovePlan failed")
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Could not approve plan (it may already be resolved).",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    "Plan approved — executing.",
				Components: []discordgo.MessageComponent{},
			},
		})
	} else {
		if err := p.rt.CancelRun(ctx, runID); err != nil {
			log.Printf("[discord plugin] CancelRun failed")
			_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "Could not deny plan (it may already be resolved).",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    "Plan denied / run cancelled.",
				Components: []discordgo.MessageComponent{},
			},
		})
	}
	p.mu.Lock()
	delete(p.planMsg, runID)
	p.mu.Unlock()
}
