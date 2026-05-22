package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/mnemos"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// scopeFromPolicy maps an AssistantDefinition.MemoryPolicy.Scope string
// to a mnemos.Scope. Empty input defaults to workspace, matching the
// behavior of the pre-ADR-0004 code path.
func scopeFromPolicy(policyScope string) mnemos.Scope {
	switch policyScope {
	case "profile":
		return mnemos.LocalProfile()
	case "preferences":
		return mnemos.LocalPreferences()
	case "", "workspace":
		return mnemos.LocalWorkspace()
	default:
		// Unknown scope — surface as workspace rather than reject. The
		// validator on the wire boundary catches malformed scopes; this
		// path is reached only with legacy policy strings.
		return mnemos.LocalWorkspace()
	}
}

// executeRun manages the run lifecycle: plan → plan_review → execute
func (r *Runtime) executeRun(ctx context.Context, run *domain.Run, assistant *domain.AssistantDefinition) {
	slog.Info("executeRun: start", "run_id", run.ID, "status", run.Status, "assistant_id", assistant.ID)
	// Phase 1: Planning (blocks until plan is approved)
	steps, err := r.executePlanningPhase(ctx, run, assistant)
	if err != nil {
		slog.Error("executeRun: planning failed", "run_id", run.ID, "error", err)
		r.failRun(ctx, run, err)
		return
	}
	slog.Info("executeRun: planning succeeded", "run_id", run.ID, "steps", len(steps))

	// Phase 2: Execution (after plan approval)
	// Refresh run state before execution since it may have been updated
	freshRun, err := r.runRepo.GetByID(run.ID)
	if err != nil {
		r.failRun(ctx, run, fmt.Errorf("failed to refresh run state: %w", err))
		return
	}
	if freshRun.Status == domain.RunFailed || freshRun.Status == domain.RunCancelled {
		return // Run was terminated during plan review
	}
	r.executeExecutionPhase(ctx, freshRun, assistant, steps)
}

// executePlanningPhase creates the plan and pauses at plan_review.
// Blocks until the plan is approved (status changes from plan_review to executing).
// Returns the created steps for later execution.
func (r *Runtime) executePlanningPhase(ctx context.Context, run *domain.Run, assistant *domain.AssistantDefinition) ([]*domain.Step, error) {
	slog.Info("planning phase: start", "run_id", run.ID)
	// Transition to planning
	if err := r.transitionRun(ctx, run, domain.RunPlanning); err != nil {
		slog.Error("planning transition failed", "run_id", run.ID, "error", err)
		return nil, fmt.Errorf("planning transition failed: %w", err)
	}

	// Load folder contexts if assistant has any
	contextData, err := r.loadFolderContexts(ctx, assistant)
	if err != nil {
		slog.Warn("failed to load folder contexts", "run_id", run.ID, "error", err)
		return nil, fmt.Errorf("failed to load folder contexts: %w", err)
	}

	// Inbound media enrichment (ADR 0001 §rich-media, task media-10).
	// Channel plugins captured attachments on inbound; the planner sees
	// a goal that includes transcripts + extracted text so the
	// assistant can reason over voice notes / documents like they
	// were typed by the user. Silent no-op when no media plugin is
	// installed or the run carries no attachments.
	if r.enrichment != nil {
		enriched := r.enrichment.Enrich(ctx, run.ID, run.Goal)
		if enriched != run.Goal {
			run.Goal = enriched
			// Persist so the UI + downstream stages see the enriched
			// version too — without this, the Chats tab would still
			// show the bare-bones original goal.
			if err := r.runRepo.Update(run); err != nil {
				return nil, fmt.Errorf("persist enriched goal: %w", err)
			}
		}
	}

	// Planning phase: create plan with step definitions
	plan, steps, err := r.planSteps(run, assistant, contextData)
	if err != nil {
		slog.Error("planning failed", "run_id", run.ID, "error", err)
		return nil, fmt.Errorf("planning failed: %w", err)
	}
	slog.Info("plan created", "run_id", run.ID, "plan_id", plan.ID, "step_count", len(steps))
	slog.Info("planning phase: plan created", "run_id", run.ID, "plan_id", plan.ID, "step_count", len(steps))

	// Update run with plan version
	run.PlanVersion = plan.Version
	if err := r.runRepo.Update(run); err != nil {
		return nil, fmt.Errorf("failed to update run plan version: %w", err)
	}

	// Publish plan proposed event
	_, _ = r.eventBus.Publish(ctx, domain.EventPlanProposed, run.ID, nil, map[string]interface{}{
		"plan_id":      plan.ID,
		"plan_version": plan.Version,
		"step_count":   len(plan.Steps),
	})

	// Transition to plan_review — wait for user approval
	if err := r.transitionRun(ctx, run, domain.RunPlanReview); err != nil {
		slog.Error("plan review transition failed", "run_id", run.ID, "error", err)
		return nil, fmt.Errorf("plan review transition failed: %w", err)
	}
	slog.Info("plan review: waiting for approval", "run_id", run.ID, "plan_id", plan.ID)

	approvalSignal := r.registerPlanApproval(run.ID)
	defer r.unregisterPlanApproval(run.ID, approvalSignal)

	// Channel wake-ups handle the common case (ApprovePlan in this process)
	// with sub-tick latency. Slow polling remains as a fallback for restart
	// scenarios where no in-memory signal exists.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	checkStatus := func() (*domain.Run, error) {
		currentRun, err := r.runRepo.GetByID(run.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to read run status: %w", err)
		}
		switch currentRun.Status {
		case domain.RunExecuting:
			return currentRun, nil
		case domain.RunFailed, domain.RunCancelled:
			return nil, fmt.Errorf("run %s while awaiting plan approval", currentRun.Status)
		case domain.RunPlanReview:
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected run status during plan review: %s", currentRun.Status)
		}
	}

	if currentRun, err := checkStatus(); err != nil {
		return nil, err
	} else if currentRun != nil {
		run.Status = currentRun.Status
		return steps, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-approvalSignal:
			currentRun, err := checkStatus()
			if err != nil {
				return nil, err
			}
			if currentRun != nil {
				run.Status = currentRun.Status
				return steps, nil
			}
		case <-ticker.C:
			currentRun, err := checkStatus()
			if err != nil {
				return nil, err
			}
			if currentRun != nil {
				// Plan approved — refresh run state and return steps.
				run.Status = currentRun.Status
				return steps, nil
			}
		}
	}
}

// executeExecutionPhase executes the planned steps.
// Called after the plan has been approved.
func (r *Runtime) executeExecutionPhase(ctx context.Context, run *domain.Run, assistant *domain.AssistantDefinition, steps []*domain.Step) {
	// Transition to executing (skip if already there — e.g. after ApprovePlan)
	if run.Status != domain.RunExecuting {
		if err := r.transitionRun(ctx, run, domain.RunExecuting); err != nil {
			r.failRun(ctx, run, fmt.Errorf("executing transition failed: %w", err))
			return
		}
	}

	// Execute steps
	pauseSignal := r.registerPauseSignal(run.ID)
	defer r.unregisterPauseSignal(run.ID, pauseSignal)

	for i := 0; i < len(steps); {
		step := steps[i]
		for {
			freshRun, err := r.runRepo.GetByID(run.ID)
			if err != nil {
				r.failRun(ctx, run, fmt.Errorf("failed to refresh run before step: %w", err))
				return
			}
			run.Status = freshRun.Status
			if freshRun.Status == domain.RunPaused {
				select {
				case <-ctx.Done():
					return
				case <-pauseSignal:
					continue
				}
			}
			if freshRun.Status == domain.RunCancelled || freshRun.Status == domain.RunFailed {
				return
			}
			break
		}

		run.CurrentStepID = &step.ID
		if err := r.runRepo.Update(run); err != nil {
			r.failRun(ctx, run, fmt.Errorf("failed to update current step: %w", err))
			return
		}

		if err := r.executeStep(ctx, run, step, assistant); err != nil {
			if errors.Is(err, errRunPaused) {
				continue
			}
			// Replan-on-failure: feed the error back into the planner
			// so it can propose a corrective plan instead of dying on
			// the first stack-trace. Bounded by MaxReplansPerRun
			// inside Replan; once exhausted the run fails normally.
			if newSteps, replanErr := r.Replan(ctx, run, step, err.Error()); replanErr == nil && len(newSteps) > 0 {
				slog.Info("replan-on-failure: planner returned corrective plan", "run_id", run.ID, "old_step", step.ID, "new_step_count", len(newSteps))
				steps = newSteps
				i = 0
				// Drop back into executing — the run already is, but
				// transitionRun is a no-op against same-state.
				continue
			} else if replanErr != nil {
				slog.Info("replan-on-failure: skipped", "run_id", run.ID, "reason", replanErr.Error())
			}
			r.failRun(ctx, run, fmt.Errorf("step %d execution failed: %w", i, err))
			return
		}

		i++
	}

	// Mark run as completed
	if err := r.transitionRun(ctx, run, domain.RunCompleted); err != nil {
		r.failRun(ctx, run, fmt.Errorf("completion transition failed: %w", err))
		return
	}

	// Store run result as memory if assistant has memory enabled
	if assistant.MemoryPolicy.Enabled && r.memClient != nil {
		var memoryContent string
		memoryContent = fmt.Sprintf("Run goal: %s\n", run.Goal)
		for _, step := range steps {
			if step.Output != "" {
				memoryContent += fmt.Sprintf("- %s: %s\n", step.Title, step.Output)
			}
		}

		scope := scopeFromPolicy(assistant.MemoryPolicy.Scope)
		_ = r.memClient.Store(ctx, scope, &mnemos.Entry{
			Content:     memoryContent,
			AssistantID: &assistant.ID,
			RunID:       &run.ID,
		})
	}

	_, _ = r.eventBus.Publish(ctx, domain.EventRunCompleted, run.ID, nil, map[string]interface{}{
		"step_count": len(steps),
	})
	r.limiter.ForgetRun(run.ID)
}

// loadFolderContexts scans folder contexts attached to the assistant
func (r *Runtime) loadFolderContexts(ctx context.Context, assistant *domain.AssistantDefinition) (string, error) {
	if len(assistant.Contexts) == 0 {
		return "", nil
	}

	var contextData string
	for _, attachment := range assistant.Contexts {
		if attachment.Type != "folder" {
			continue
		}

		result := r.toolExecutor.Execute(ctx, "filesystem.context", map[string]interface{}{
			"path":      attachment.Path,
			"max_depth": 3,
		})

		if !result.Success {
			return "", fmt.Errorf("failed to load context for %s: %s", attachment.Path, result.Error)
		}

		// Format the context data
		contextData += fmt.Sprintf("\n--- Context: %s ---\n", attachment.Path)
		if tree, ok := result.Output["tree"].(*tools.FileNode); ok {
			contextData += formatFileNode(tree, 0)
		}
		if stats, ok := result.Output["stats"].(map[string]interface{}); ok {
			contextData += fmt.Sprintf("\nStats: %v files, %v dirs, %v bytes total\n",
				stats["file_count"], stats["dir_count"], stats["total_size"])
		}
	}

	return contextData, nil
}

// planStepTitle renders a short human-readable step title. Uses the tool
// name as a prefix so plan-review UIs can scan "Think about X" vs "Run X"
// without parsing the full description.
func planStepTitle(goal, tool string) string {
	verb := "Execute"
	switch tool {
	case "llm.chat":
		verb = "Think about"
	case "filesystem.read":
		verb = "Read"
	case "filesystem.write":
		verb = "Write"
	case "filesystem.patch":
		verb = "Patch"
	case "command.exec":
		verb = "Run"
	}
	return fmt.Sprintf("%s: %s", verb, goal)
}

// formatFileNode formats a file tree node as a string
func formatFileNode(node *tools.FileNode, depth int) string {
	if node == nil {
		return ""
	}

	indent := ""
	for i := 0; i < depth; i++ {
		indent += "  "
	}

	result := fmt.Sprintf("%s%s/\n", indent, node.Name)
	for _, child := range node.Children {
		result += formatFileNode(child, depth+1)
	}
	return result
}

// planSteps creates a plan and its corresponding executable steps.
//
// When a default LLM provider is configured, asks the planner for a
// multi-step decomposition (feature #33). The planner returns nil on any
// failure (unparseable JSON, unknown tool proposed, rate-limited, etc.);
// in that case we fall back to the simple single-step shape so the run
// still makes progress with the legacy behavior.
//
// When no LLM is configured at all, defaults to command.exec — the
// developer-sandbox mode for CI and local testing without a provider.
func (r *Runtime) planSteps(run *domain.Run, assistant *domain.AssistantDefinition, contextData string) (*domain.Plan, []*domain.Step, error) {
	version, err := r.planRepo.GetPlanVersion(run.ID)
	if err != nil {
		version = 1
	}

	input := run.Goal
	if contextData != "" {
		input = fmt.Sprintf("%s\n\nAttached context:\n%s", run.Goal, contextData)
	}

	plan := &domain.Plan{
		ID:        uuid.New().String(),
		RunID:     run.ID,
		Version:   version,
		CreatedAt: time.Now().UTC(),
	}

	// Try LLM-driven planning first. Uses a bounded context so a slow
	// planner call doesn't block the run indefinitely.
	plannerCtx, cancel := context.WithTimeout(r.rootCtx, 30*time.Second)
	defer cancel()
	plannedSteps := r.planWithLLM(plannerCtx, run.Goal, assistant, contextData)

	var stepDefs []domain.StepDefinition
	if len(plannedSteps) > 0 {
		stepDefs = plannedStepsToDefinitions(plannedSteps, plan.ID)
	} else {
		// Fallback single-step plan. Pick the right tool based on whether
		// an LLM is configured at all.
		tool := "command.exec"
		description := "Run the user's goal as a shell command"
		if r.hasDefaultLLM() {
			tool = "llm.chat"
			description = "Ask the LLM to respond to the user's goal"
		}
		stepDefs = []domain.StepDefinition{{
			ID:                 uuid.New().String(),
			PlanID:             plan.ID,
			Title:              planStepTitle(run.Goal, tool),
			Description:        description,
			ExpectedTool:       tool,
			ExpectedCapability: tool,
			Order:              0,
			CreatedAt:          time.Now().UTC(),
		}}
	}
	plan.Steps = stepDefs

	if err := r.planRepo.Create(plan); err != nil {
		return nil, nil, fmt.Errorf("failed to create plan: %w", err)
	}

	// Create a corresponding executable Step for every StepDefinition.
	// For the fallback (single-step) shape, Input is the full goal + any
	// workspace context. For planner-produced multi-step plans, each
	// step's Input is its planner-provided description when available
	// (that's the LLM's per-step intent), falling back to the goal+context.
	steps := make([]*domain.Step, 0, len(stepDefs))
	plannerProduced := len(plannedSteps) > 0
	for _, def := range stepDefs {
		defID := def.ID
		stepInput := input
		if plannerProduced && def.Description != "" {
			stepInput = def.Description
		}
		step := &domain.Step{
			ID:               uuid.New().String(),
			RunID:            run.ID,
			StepDefinitionID: &defID,
			Title:            def.Title,
			Status:           domain.StepPending,
			Input:            stepInput,
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
		}
		if err := r.stepRepo.Create(step); err != nil {
			return nil, nil, fmt.Errorf("failed to create step: %w", err)
		}
		steps = append(steps, step)
	}

	return plan, steps, nil
}

// plannedStepsToDefinitions adapts the planner's intermediate plannerStep
// shape (LLM-wire DTO with JSON tags tuned for the prompt) into the
// persistence aggregate StepDefinition. Centralised here so the seam
// between "what the planner produced" and "what gets stored + executed"
// is one named function rather than inlined construction logic — when
// we add a second planner backend (or rework arguments to be typed per
// tool) this is the only adapter site that needs touching.
//
// Linear dependency chain: each step depends on the previous one. The
// graph-shaped "branching" plan is built by EditPlan, not here.
func plannedStepsToDefinitions(plannedSteps []plannerStep, planID string) []domain.StepDefinition {
	defs := make([]domain.StepDefinition, 0, len(plannedSteps))
	for i, ps := range plannedSteps {
		deps := []string{}
		if i > 0 {
			deps = []string{defs[i-1].ID}
		}
		defs = append(defs, domain.StepDefinition{
			ID:                 uuid.New().String(),
			PlanID:             planID,
			Title:              truncateTitle(ps.Title),
			Description:        ps.Description,
			ExpectedTool:       ps.Tool,
			ExpectedCapability: ps.Tool,
			Why:                ps.Why,
			Arguments:          ps.Arguments,
			Order:              i,
			DependsOn:          deps,
			CreatedAt:          time.Now().UTC(),
		})
	}
	return defs
}

// truncateTitle enforces the 80-char ceiling the planner prompt asks for.
// The runtime cap is the source of truth; the prompt just nudges the LLM
// toward shorter titles. Anything over the cap is sliced and an ellipsis
// appended so the UI never has to lay out a multi-line plan-row title.
func truncateTitle(title string) string {
	const max = 80
	title = strings.TrimSpace(title)
	if len(title) <= max {
		return title
	}
	return title[:max-1] + "…"
}
