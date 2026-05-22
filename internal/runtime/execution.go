package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

var errRunPaused = errors.New("run paused")

type rememberedApproval struct {
	Approved      bool      `json:"approved"`
	ExpiresAt     time.Time `json:"expires_at"`
	SafetyProfile string    `json:"safety_profile"`
}

// executeStep executes a single step
func (r *Runtime) executeStep(ctx context.Context, run *domain.Run, step *domain.Step, assistant *domain.AssistantDefinition) error {
	slog.Info("step: start execution", "run_id", run.ID, "step_id", step.ID, "title", step.Title, "status", step.Status)
	// Step start transitions differ for fresh vs resumed execution.
	switch step.Status {
	case domain.StepPending:
		if err := r.transitionStep(ctx, step, domain.StepReady); err != nil {
			return err
		}
		if err := r.transitionStep(ctx, step, domain.StepRunning); err != nil {
			return err
		}
	case domain.StepReady, domain.StepBlocked:
		if err := r.transitionStep(ctx, step, domain.StepRunning); err != nil {
			return err
		}
	case domain.StepRunning:
		// No-op: resumed flow may already have this status in memory.
	default:
		return fmt.Errorf("step is not executable from state %s", step.Status)
	}

	// Publish step started event
	_, _ = r.eventBus.Publish(ctx, domain.EventStepStarted, run.ID, &step.ID, map[string]interface{}{
		"title": step.Title,
	})

	// Resolve the StepDefinition (if any) once at entry. Both determineTool
	// and buildToolInput need it; loading twice was an extra SQL query per
	// step with no upside. nil here means a legacy step from before #34;
	// downstream code already nil-checks.
	def := r.stepDefinitionFor(step)

	// Determine what tool to execute. Prefer the StepDefinition's
	// ExpectedTool (set by planSteps or EditPlan); fall back to the
	// legacy heuristic for old step rows written before #34 landed.
	toolName := toolFromDef(def)
	capability := r.getCapabilityForTool(toolName)

	// Resolve the tool input upfront so the approval signature reflects the
	// actual arguments the step will run with — not just the natural-language
	// step.Input — and so the approval card can show the user the same
	// values. Without this, a remembered "approve writes to notes.md"
	// would silently apply to a later step whose path was /etc/hosts.
	toolInput := buildToolInputFromDef(step, assistant, toolName, def)
	inputSig := approvalInputSignature(capability, toolInput)

	// Ceiling check first so the user-facing error names the *real*
	// reason. effectivePermissionMode also performs this check and
	// returns PermissionDeny on violation, but we want a distinct
	// message: a missing declared-capability is fixed in the assistant
	// builder ("tick the Command box"), not in the permission policy.
	if !declaredCapabilityCeiling(assistant.Capabilities, capability) {
		slog.Warn("ceiling violation", "run_id", run.ID, "step_id", step.ID, "capability", capability)
		step.Status = domain.StepFailed
		family := permissions.SystemCapabilityFamilies[capability]
		msg := fmt.Sprintf(
			"This assistant isn't allowed to use %s because %s isn't ticked in its capabilities. Open the assistant builder and tick %s.",
			capability, family, family,
		)
		step.Error = strPtr(msg)
		if err := r.stepRepo.Update(step); err != nil {
			return fmt.Errorf("failed to update step: %w", err)
		}
		_, _ = r.eventBus.Publish(ctx, domain.EventStepFailed, run.ID, &step.ID, map[string]interface{}{
			"error":      msg,
			"capability": capability,
			"reason":     "ceiling_violation",
		})
		return &domain.UserError{
			Code:    domain.ErrCodeCeilingViolation,
			Title:   "Capability not allowed",
			Message: msg,
			Action:  "Open Assistant Builder",
		}
	}

	// Evaluate permission
	mode := r.effectivePermissionMode(run, assistant, capability)
	slog.Info("permission check", "run_id", run.ID, "step_id", step.ID, "capability", capability, "mode", mode)

	switch mode {
	case domain.PermissionDeny:
		slog.Warn("permission denied", "run_id", run.ID, "step_id", step.ID, "capability", capability)
		step.Status = domain.StepFailed
		msg := fmt.Sprintf("This action (%s) is blocked by the assistant's permission policy. Open the assistant builder and change the rule to Allow or Confirm.", capability)
		step.Error = strPtr(msg)
		if err := r.stepRepo.Update(step); err != nil {
			return fmt.Errorf("failed to update step: %w", err)
		}
		_, _ = r.eventBus.Publish(ctx, domain.EventStepFailed, run.ID, &step.ID, map[string]interface{}{
			"error":      msg,
			"capability": capability,
			"reason":     "policy_deny",
		})
		return &domain.UserError{
			Code:    domain.ErrCodePolicyDeny,
			Title:   "Action blocked",
			Message: msg,
			Action:  "Open Assistant Builder",
		}

	case domain.PermissionConfirm:
		if approved, remembered := r.rememberedDecision(assistant.ID, capability, inputSig); remembered {
			if !approved {
				slog.Info("approval remembered as denied", "run_id", run.ID, "step_id", step.ID, "capability", capability)
				msg := "This action was previously denied and Nomi remembered your choice. You can change this in the Approvals tab."
				step.Error = strPtr(msg)
				if err := r.transitionStepAtomic(ctx, step, domain.StepFailed,
					domain.EventStepFailed,
					map[string]interface{}{
						"error":      msg,
						"capability": capability,
						"reason":     "approval_remembered",
					}); err != nil {
					return fmt.Errorf("failed to finalize step after remembered denial: %w", err)
				}
				return &domain.UserError{
					Code:    domain.ErrCodeApprovalRemembered,
					Title:   "Action remembered as denied",
					Message: msg,
					Action:  "Open Approvals",
				}
			}
			break
		}

		// Transition run to awaiting approval
		if err := r.transitionRun(ctx, run, domain.RunAwaitingApproval); err != nil {
			slog.Warn("approval transition failed", "run_id", run.ID, "step_id", step.ID, "error", err)
			return fmt.Errorf("failed to transition run to awaiting approval: %w", err)
		}
		slog.Info("approval required", "run_id", run.ID, "step_id", step.ID, "capability", capability)

		// Transition step to blocked
		if err := r.transitionStep(ctx, step, domain.StepBlocked); err != nil {
			return fmt.Errorf("failed to transition step to blocked: %w", err)
		}

		// Request approval
		approval, err := r.approvalMgr.RequestApproval(ctx, run.ID, &step.ID, capability, map[string]interface{}{
			"tool":            toolName,
			"input":           step.Input,
			"assistant_id":    assistant.ID,
			"input_signature": inputSig,
		})
		if err != nil {
			// Rollback state on error
			_ = r.transitionRun(ctx, run, domain.RunExecuting)
			_ = r.transitionStep(ctx, step, domain.StepRunning)
			return fmt.Errorf("approval request failed: %w", err)
		}

		// Wait for approval
		status, err := r.approvalMgr.WaitForResolution(ctx, approval.ID)
		if err != nil {
			// Rollback state on error
			_ = r.transitionRun(ctx, run, domain.RunExecuting)
			_ = r.transitionStep(ctx, step, domain.StepRunning)
			return fmt.Errorf("approval wait failed: %w", err)
		}

		if status == permissions.ApprovalDenied {
			msg := "You denied this action in the approval prompt. If you change your mind, you can retry the task."
			step.Error = strPtr(msg)
			if err := r.transitionStepAtomic(ctx, step, domain.StepFailed,
				domain.EventStepFailed,
				map[string]interface{}{
					"error":      msg,
					"capability": capability,
					"reason":     "approval_denied",
				}); err != nil {
				return fmt.Errorf("failed to finalize step after approval denial: %w", err)
			}
			return &domain.UserError{
				Code:    domain.ErrCodeApprovalDenied,
				Title:   "Action denied",
				Message: msg,
				Action:  "Retry Task",
			}
		}

		// Approved - resume execution: transition back to executing/running
		if err := r.transitionRun(ctx, run, domain.RunExecuting); err != nil {
			return fmt.Errorf("failed to transition run back to executing: %w", err)
		}
		if err := r.transitionStep(ctx, step, domain.StepRunning); err != nil {
			return fmt.Errorf("failed to transition step back to running: %w", err)
		}
		// Continue execution below
	}

	// Re-fetch the assistant policy before invoking the tool. Approval
	// flows that block on user input release the connection (and the
	// goroutine is suspended on a Go channel), so a concurrent
	// UpdateAssistant landing during the wait would leave us evaluating
	// against a stale policy. Re-checking here closes that TOCTOU window
	// without rejecting the user's just-given approval — they consented to
	// the capability, they didn't consent to it being demoted to deny
	// while they were typing.
	if fresh, err := r.assistantRepo.GetByID(assistant.ID); err == nil && fresh != nil {
		mode := r.permEngine.Evaluate(fresh.PermissionPolicy, capability)
		if mode == domain.PermissionDeny {
			msg := fmt.Sprintf("This action (%s) is now blocked by the assistant's permission policy. The policy was changed while this step was waiting.", capability)
			step.Error = strPtr(msg)
			if err := r.transitionStepAtomic(ctx, step, domain.StepFailed,
				domain.EventStepFailed,
				map[string]interface{}{
					"error":      msg,
					"capability": capability,
					"reason":     "policy_deny_after_approval",
				}); err != nil {
				return fmt.Errorf("failed to finalize step after late policy deny: %w", err)
			}
			return &domain.UserError{
				Code:    domain.ErrCodePolicyDeny,
				Title:   "Action blocked",
				Message: msg,
				Action:  "Open Assistant Builder",
			}
		}
		// Refresh the in-memory copy so the constraint-merge below sees the
		// updated rule set if the policy was edited (but not to deny).
		assistant = fresh
	}

	// Per-run tool-call rate limit. Runs that saturate this budget are
	// almost always stuck in a loop; returning an error here fails the
	// step with a clear message rather than silently burning CPU.
	if !r.limiter.AllowToolCall(run.ID) {
		msg := "This task is making too many tool calls too quickly — it may be stuck in a loop. Try rephrasing your request."
		step.Error = strPtr(msg)
		_ = r.transitionStepAtomic(ctx, step, domain.StepFailed,
			domain.EventStepFailed,
			map[string]interface{}{
				"error":      msg,
				"capability": capability,
				"reason":     "rate_limited",
			})
		return &domain.UserError{
			Code:    domain.ErrCodeRateLimited,
			Title:   "Too many actions",
			Message: msg,
		}
	}

	// Per-capability constraints from the permission rule flow into the tool
	// as named keys. The tool side is the authority on what each constraint
	// means (command.exec honors "allowed_binaries"); the runtime just
	// passes them through. This is how "granular" permissions avoid
	// baking tool-specific logic into the engine.
	if rule := r.permEngine.MatchingRule(assistant.PermissionPolicy, capability); rule != nil {
		for k, v := range rule.Constraints {
			// Runtime reserves workspace_root + command keys; never let
			// user-declared constraints override them.
			if k == "workspace_root" || k == "command" {
				continue
			}
			toolInput[k] = v
		}
	}
	// Wire a delta callback so streaming-capable tools (today: llm.chat
	// against the OpenAI-compat adapter) emit step.streaming events as
	// tokens arrive. The toolInput key starts with double-underscore so
	// it's clearly an internal escape hatch rather than user data; tools
	// that don't recognise it ignore it. Skipped at signature time so
	// the approval signature isn't a function pointer.
	if toolName == "llm.chat" {
		seq := 0
		runID, stepID := run.ID, step.ID
		toolInput["__on_delta"] = func(delta string) {
			seq++
			_, _ = r.eventBus.Publish(ctx, domain.EventStepStreaming, runID, &stepID, map[string]interface{}{
				"delta": delta,
				"seq":   seq,
			})
		}
	}

	// Resolve the sandbox backend for command.exec — picks the assistant's
	// configured backend (default local) and injects it via a reserved
	// __sandbox key, matching the __on_delta escape-hatch pattern. Skipped
	// at signature time so the approval signature isn't a backend pointer.
	if toolName == "command.exec" && r.executorRegistry != nil {
		if backend := r.executorRegistry.Resolve(assistant.ExecutorBackend); backend != nil {
			toolInput["__sandbox"] = backend
			if assistant.SandboxImage != "" {
				toolInput["__sandbox_image"] = assistant.SandboxImage
			}
		}
	}

	// Retry loop. Transient failures (network timeouts, 5xx from upstream,
	// rate limits) retry with exponential backoff up to Runtime.maxRetries.
	// Deterministic failures (permission denied, missing binary, refused
	// shell metacharacter) fail immediately — retrying won't fix them.
	result := r.invokeWithRetry(ctx, run, step, toolName, toolInput, capability)

	// Pause requested while this step was running: mark it blocked and let the
	// execution loop re-invoke it after ResumeRun.
	freshRun, err := r.runRepo.GetByID(run.ID)
	if err == nil && freshRun.Status == domain.RunPaused {
		_ = r.transitionStep(ctx, step, domain.StepBlocked)
		return errRunPaused
	}

	if result.Success {
		if result.Output != nil {
			if output, ok := result.Output["output"].(string); ok {
				step.Output = output
			}
		}
		if err := r.transitionStepAtomic(ctx, step, domain.StepDone,
			domain.EventStepCompleted,
			map[string]interface{}{"output": step.Output},
		); err != nil {
			return fmt.Errorf("failed to finalize completed step: %w", err)
		}
	} else {
		step.Error = &result.Error
		if err := r.transitionStepAtomic(ctx, step, domain.StepFailed,
			domain.EventStepFailed,
			map[string]interface{}{
				"error":          result.Error,
				"capability":     capability,
				"retry_attempts": step.RetryCount,
			},
		); err != nil {
			return fmt.Errorf("failed to finalize failed step: %w", err)
		}
		return &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Action failed",
			Message: result.Error,
		}
	}

	return nil
}

// invokeWithRetry wraps toolExecutor.Execute with transient-failure retry
// + exponential backoff. Writes step.RetryCount + publishes step.retrying
// events so the UI can show "retrying (2/3)" rather than a static "failed".
//
// Retry budget is per-step-attempt, not per-run: a step that retries 3
// times and then succeeds consumes 3 retries. A later step on the same run
// starts with a fresh budget.
func (r *Runtime) invokeWithRetry(
	ctx context.Context,
	run *domain.Run,
	step *domain.Step,
	toolName string,
	toolInput map[string]interface{},
	capability string,
) *tools.ExecutionResult {
	var result *tools.ExecutionResult
	maxAttempts := r.maxRetries + 1 // maxRetries=3 → up to 4 total attempts (1 + 3 retries)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result = r.toolExecutor.Execute(ctx, toolName, toolInput)
		if result.Success {
			return result
		}
		// Last attempt, or the failure is deterministic — stop retrying.
		if attempt == maxAttempts-1 || !isTransientFailure(result.Error) {
			return result
		}
		// Budget remaining and failure looks retryable. Record the attempt
		// and publish an observable event so the UI can reflect progress.
		step.RetryCount++
		_ = r.stepRepo.Update(step)
		_, _ = r.eventBus.Publish(ctx, domain.EventStepRetrying, run.ID, &step.ID, map[string]interface{}{
			"attempt":    step.RetryCount,
			"max":        r.maxRetries,
			"error":      result.Error,
			"capability": capability,
		})

		// Exponential backoff: 100ms, 200ms, 400ms, 800ms. Cancellable
		// via the run context so Shutdown doesn't wait on a sleeping step.
		backoff := time.Duration(100*(1<<attempt)) * time.Millisecond
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return result
		}
	}
	return result
}

// isTransientFailure classifies tool-execution errors into retryable vs
// not. Keep the allowlist tight — the default is "don't retry," so only
// errors we've actually seen resolve on retry make the list.
func isTransientFailure(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	// Network + upstream transient markers.
	transientMarkers := []string{
		"timeout",
		"timed out",
		"connection refused",
		"connection reset",
		"temporary failure",
		"try again",
		"rate limit",
		"rate_limit",
		"429",
		"500 ",
		"502 ",
		"503 ",
		"504 ",
		"i/o timeout",
		"eof",
	}
	for _, m := range transientMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// approvalInputSignature derives the cache key for "remember this decision"
// from the capability plus the resolved tool arguments the step will run
// with. Hashing only the natural-language step.Input was insufficient: a
// remembered approval for "write notes.md" would silently apply to a later
// step whose plan-injected path was /etc/hosts. Excludes runtime-controlled
// keys (workspace_root, system_prompt) so changing the workspace folder or
// editing the assistant persona doesn't invalidate every prior remembered
// decision; those keys aren't user-visible in the approval card.
func approvalInputSignature(capability string, toolInput map[string]interface{}) string {
	const (
		// Keys the user approves explicitly via the approval card. Anything
		// outside this set is either runtime plumbing (workspace_root) or
		// derived from authoritative state (system_prompt comes from the
		// assistant definition, not the planner).
		_ = "" // documentation-only constant placeholder
	)
	signed := make(map[string]interface{}, len(toolInput))
	for k, v := range toolInput {
		switch k {
		case "workspace_root", "system_prompt":
			continue
		}
		signed[k] = v
	}
	canon, err := json.Marshal(canonicalKeys(signed))
	if err != nil {
		// Fall back to a still-deterministic shape so a remembered decision
		// keyed on this hash will at least be stable across calls; failing
		// hard here would force every approval to re-prompt unnecessarily.
		canon = []byte(fmt.Sprintf("%v", signed))
	}
	h := sha256.Sum256(append([]byte(capability+"|"), canon...))
	return hex.EncodeToString(h[:])
}

// canonicalKeys returns the input map with keys sorted lexicographically
// using json.Marshal's deterministic key order on map[string]interface{}.
// json.Marshal already sorts map keys, so this is a no-op wrapper kept to
// document the requirement at the call site.
func canonicalKeys(m map[string]interface{}) map[string]interface{} {
	return m
}

func rememberedApprovalKey(assistantID, capability, inputSignature string) string {
	return fmt.Sprintf("approval.remember.%s.%s.%s", assistantID, capability, inputSignature)
}

func (r *Runtime) rememberedDecision(assistantID, capability, inputSignature string) (bool, bool) {
	if r.settingsRepo == nil || assistantID == "" || capability == "" || inputSignature == "" {
		return false, false
	}
	raw, err := r.settingsRepo.Get(rememberedApprovalKey(assistantID, capability, inputSignature))
	if err != nil {
		return false, false
	}
	var remembered rememberedApproval
	if err := json.Unmarshal([]byte(raw), &remembered); err != nil {
		return false, false
	}
	if time.Now().UTC().After(remembered.ExpiresAt) {
		return false, false
	}
	currentProfile := r.settingsRepo.GetOrDefault("safety_profile", permissions.DefaultSafetyProfile)
	if remembered.SafetyProfile != "" && remembered.SafetyProfile != currentProfile {
		return false, false
	}
	return remembered.Approved, true
}

// determineTool decides which tool to route the step to. Since #34, the
// source of truth is StepDefinition.ExpectedTool — planSteps writes it
// when producing plans, and EditPlan lets users change it. If the step
// predates multi-step planning and has no definition attached, fall back
// to command.exec for backwards compatibility.
//
// Kept as a thin wrapper over toolFromDef so callers that don't already
// have the StepDefinition in hand pay one query for the lookup. Callers
// inside executeStep should use toolFromDef directly with the cached def.
func (r *Runtime) determineTool(step *domain.Step) string {
	return toolFromDef(r.stepDefinitionFor(step))
}

// toolFromDef extracts the tool name from a (possibly nil) StepDefinition,
// returning the legacy command.exec default when there is no definition or
// the definition's ExpectedTool was never written.
func toolFromDef(def *domain.StepDefinition) string {
	if def == nil || def.ExpectedTool == "" {
		return "command.exec"
	}
	return def.ExpectedTool
}

// buildToolInputFromDef assembles the canonical input map for a step's
// tool call from a pre-fetched StepDefinition. Built once per executeStep
// so the approval signature, the approval card payload, and the actual
// tool invocation all see the same arguments.
//
// Layering, lowest precedence first:
//  1. step.Input as a "command" fallback for legacy callers that didn't
//     route through the planner.
//  2. workspace_root, derived from the assistant's folder context. Always
//     runtime-controlled — the planner can't override it.
//  3. llm.chat plumbing (prompt, system_prompt). system_prompt is sourced
//     from assistant.SystemPrompt and is NOT planner-overridable.
//  4. Planner-emitted Arguments, gated by plannerArgumentAllowlist so a
//     misbehaving planner can't reach into reserved keys.
//
// Per-capability constraint keys (allowed_binaries, etc.) are merged in
// at execution time, AFTER this function runs, so they always win over
// planner arguments. That layering is intentional: the policy rule is
// the authoritative source for what a capability is allowed to do.
func buildToolInputFromDef(
	step *domain.Step,
	assistant *domain.AssistantDefinition,
	toolName string,
	def *domain.StepDefinition,
) map[string]interface{} {
	toolInput := map[string]interface{}{
		"command": step.Input,
	}
	if root := assistantWorkspaceRoot(assistant); root != "" {
		toolInput["workspace_root"] = root
	}
	if toolName == "llm.chat" {
		toolInput["prompt"] = step.Input
		if assistant != nil && assistant.SystemPrompt != "" {
			toolInput["system_prompt"] = assistant.SystemPrompt
		}
	}
	if def != nil && len(def.Arguments) > 0 {
		allowed := plannerArgumentAllowlist(toolName)
		for k, v := range def.Arguments {
			if !allowed[k] {
				continue
			}
			toolInput[k] = v
		}
	}
	return toolInput
}

// plannerArgumentAllowlist returns the set of toolInput keys a planner is
// permitted to set for the given tool. Derived from argumentSchemas so the
// schema (declared once) stays the source of truth. A planner that ignored
// the prompt and emitted reserved keys (workspace_root, allowed_binaries,
// system_prompt, timeout, ...) has its extras dropped at merge time.
func plannerArgumentAllowlist(toolName string) map[string]bool {
	schema, ok := argumentSchemas[toolName]
	if !ok {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(schema.allowed))
	for k := range schema.allowed {
		out[k] = true
	}
	return out
}

// stepDefinitionFor loads the StepDefinition row a runtime Step was created
// from, or nil if the step predates multi-step planning. Centralised so
// determineTool and stepArguments share one nil-safe lookup.
func (r *Runtime) stepDefinitionFor(step *domain.Step) *domain.StepDefinition {
	if step == nil || step.StepDefinitionID == nil || *step.StepDefinitionID == "" {
		return nil
	}
	def, err := r.planRepo.GetStepDefinition(*step.StepDefinitionID)
	if err != nil {
		return nil
	}
	return def
}

// getCapabilityForTool returns the capability required for a tool.
// When tool-name and capability-name diverge (e.g. filesystem.context uses
// filesystem.read), the mapping goes here rather than duplicating the
// literal string in every call site.
func (r *Runtime) getCapabilityForTool(toolName string) string {
	switch toolName {
	case "filesystem.read":
		return "filesystem.read"
	case "filesystem.write":
		return "filesystem.write"
	case "filesystem.patch":
		// Patch shares the write capability so a single permission rule
		// covers both write and patch. Operators don't need to add a
		// separate filesystem.patch rule per assistant.
		return "filesystem.write"
	case "command.exec":
		return "command.exec"
	default:
		return toolName
	}
}

// assistantWorkspaceRoot picks the assistant's first declared folder context
// as the workspace root for sandboxed tool invocations. Returns "" when the
// assistant has no folder attached, in which case filesystem.read/write will
// refuse (by design) and command.exec will fall back to the daemon's cwd.
func assistantWorkspaceRoot(assistant *domain.AssistantDefinition) string {
	if assistant == nil {
		return ""
	}
	for _, ctx := range assistant.Contexts {
		if ctx.Type == "folder" && ctx.Path != "" {
			return ctx.Path
		}
	}
	return ""
}
