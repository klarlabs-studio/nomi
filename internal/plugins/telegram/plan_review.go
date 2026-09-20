package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"bytes"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
)

const (
	callbackPlanApprove = "nomi_plan_approve:"
	callbackPlanDeny    = "nomi_plan_deny:"
	maxPlanStepsInMsg   = 5
)

// subscribePlanReview listens for plan.proposed (and terminal/start
// signals that clear stale buttons) so Telegram chats can approve or
// deny a plan without opening the desktop app — mirroring the
// approval.requested UX already shipped for tool approvals.
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

// onPlanProposed posts (or updates) an inline-keyboard plan review
// prompt in the originating Telegram chat. Silently no-ops when the
// run isn't Telegram-originated.
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
	conn, err := p.connections.GetByID(conv.ConnectionID)
	if err != nil || conn == nil {
		return
	}
	token, err := p.resolveBotToken(conn)
	if err != nil {
		return
	}

	text := formatPlanReviewText(run.Goal, plan)
	requiresDesktop := planRequiresDesktopReview(plan)

	p.mu.Lock()
	ref, haveRef := p.planMsg[run.ID]
	p.mu.Unlock()

	edited, _ := evt.Payload["edited"].(bool)
	if edited && haveRef {
		if err := p.editPlanPrompt(ctx, token, ref.ChatID, ref.MessageID, text, run.ID, requiresDesktop); err != nil {
			log.Printf("[telegram plugin] edit plan prompt: %v", err)
		}
		return
	}

	msgID, err := p.sendPlanPrompt(ctx, token, conv.ExternalConversationID, text, run.ID, requiresDesktop)
	if err != nil {
		log.Printf("[telegram plugin] post plan prompt: %v", err)
		return
	}
	p.mu.Lock()
	p.planMsg[run.ID] = approvalMsgRef{
		ConnectionID: conv.ConnectionID,
		ChatID:       conv.ExternalConversationID,
		MessageID:    msgID,
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
	if err != nil {
		return
	}
	token, err := p.resolveBotToken(conn)
	if err != nil {
		return
	}
	_ = p.editMessageText(ctx, token, ref.ChatID, ref.MessageID, label)
}

// formatPlanReviewText builds the Markdown body for a plan-review prompt.
func formatPlanReviewText(goal string, plan *domain.Plan) string {
	var b strings.Builder
	b.WriteString("*Plan ready for review*\n")
	if goal != "" {
		b.WriteString("Goal: ")
		b.WriteString(escapeTelegramMarkdown(truncateRunes(goal, 200)))
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
		b.WriteString(fmt.Sprintf("%d. %s", i+1, escapeTelegramMarkdown(truncateRunes(title, 80))))
		cap := s.ExpectedCapability
		if cap == "" {
			cap = s.ExpectedTool
		}
		if cap != "" {
			b.WriteString(fmt.Sprintf(" — `%s`", escapeTelegramMarkdown(cap)))
		}
		b.WriteString("\n")
	}
	if len(steps) > maxPlanStepsInMsg {
		b.WriteString(fmt.Sprintf("(+%d more)\n", len(steps)-maxPlanStepsInMsg))
	}
	if planRequiresDesktopReview(plan) {
		b.WriteString("\n_This plan writes files or runs shell/mutating tools. Approve in the Nomi desktop app to review diffs._")
	} else {
		b.WriteString("\nApprove to execute. Deny cancels the run.")
	}
	return b.String()
}

// planRequiresDesktopReview mirrors tray gating: write/patch/irreversible
// shell and mutate-shaped MCP tools force in-app Review so DiffPreview
// stays in the loop.
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

func escapeTelegramMarkdown(s string) string {
	// Minimal escaping for Telegram legacy Markdown: *, _, `, [
	replacer := strings.NewReplacer(
		"*", "\\*",
		"_", "\\_",
		"`", "\\`",
		"[", "\\[",
	)
	return replacer.Replace(s)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (p *Plugin) sendPlanPrompt(ctx context.Context, token, chatID, text, runID string, requiresDesktop bool) (int, error) {
	row := []map[string]string{}
	if !requiresDesktop {
		row = append(row, map[string]string{
			"text": "Approve plan", "callback_data": callbackPlanApprove + runID,
		})
	}
	row = append(row, map[string]string{
		"text": "Deny plan", "callback_data": callbackPlanDeny + runID,
	})
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
		"reply_markup": map[string]interface{}{
			"inline_keyboard": [][]map[string]string{row},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", p.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("telegram sendMessage returned %d", resp.StatusCode)
	}
	var decoded struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil || !decoded.OK {
		return 0, fmt.Errorf("decode sendMessage response")
	}
	return decoded.Result.MessageID, nil
}

func (p *Plugin) editPlanPrompt(ctx context.Context, token, chatID string, messageID int, text, runID string, requiresDesktop bool) error {
	row := []map[string]string{}
	if !requiresDesktop {
		row = append(row, map[string]string{
			"text": "Approve plan", "callback_data": callbackPlanApprove + runID,
		})
	}
	row = append(row, map[string]string{
		"text": "Deny plan", "callback_data": callbackPlanDeny + runID,
	})
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "Markdown",
		"reply_markup": map[string]interface{}{
			"inline_keyboard": [][]map[string]string{row},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/editMessageText", p.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram editMessageText returned %d", resp.StatusCode)
	}
	return nil
}
