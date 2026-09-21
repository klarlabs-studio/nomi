package runtime

import (
	"context"
	"log/slog"

	"go.klarlabs.de/nomi/internal/domain"
)

const autoApproveSafePlansKey = "auto_approve_safe_plans"

// Messaging plugins that may auto-approve a safe plan. Desktop / CLI
// runs have no conversation (or a non-messaging plugin) and always wait.
var messagingChannelPlugins = map[string]bool{
	"com.nomi.telegram": true,
	"com.nomi.slack":    true,
	"com.nomi.discord":  true,
	"com.nomi.whatsapp": true,
	"com.nomi.email":    true,
	"com.nomi.matrix":   true,
}

// maybeAutoApproveSafeChannelPlan approves a plan that does not need
// desktop DiffPreview when the user opted in and the run came from a
// messaging channel. Returns true when ApprovePlan succeeded.
func (r *Runtime) maybeAutoApproveSafeChannelPlan(ctx context.Context, run *domain.Run, plan *domain.Plan) bool {
	if r.settingsRepo == nil || run == nil || plan == nil {
		return false
	}
	if r.settingsRepo.GetOrDefault(autoApproveSafePlansKey, "false") != "true" {
		return false
	}
	if run.ConversationID == nil || *run.ConversationID == "" || r.conversationRepo == nil {
		return false
	}
	conv, err := r.conversationRepo.GetByID(*run.ConversationID)
	if err != nil || conv == nil || !messagingChannelPlugins[conv.PluginID] {
		return false
	}
	if domain.PlanRequiresDesktopReview(plan) {
		return false
	}
	if err := r.ApprovePlan(ctx, run.ID); err != nil {
		slog.Warn("safe plan auto-approve failed", "run_id", run.ID, "error", err)
		return false
	}
	return true
}
