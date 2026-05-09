// Package runtime implements the orchestration layer for Nomi: it creates
// runs, drives them through the plan → execute lifecycle, gates every tool
// call against the permission engine (intersected with the source
// connector's manifest when applicable), and persists every state change as
// an event.
//
// The package is intentionally split across several files:
//
//   - engine.go       — Runtime struct, constructors, config, and the
//     public API surface (CreateRun, GetRun, RetryRun, …).
//   - lifecycle.go    — executeRun / executePlanningPhase /
//     executeExecutionPhase / planSteps / folder-context
//     loading. The internals of the planning pipeline.
//   - execution.go    — executeStep plus the tool-selection helpers and the
//     assistant→workspace-root mapping.
//   - transitions.go  — state-machine wrappers (transitionRun,
//     transitionStep, transitionStepAtomic, failRun).
//   - permissions.go  — effectivePermissionMode and intersectModes; the
//     assistant∩manifest intersection logic.
//   - ratelimit.go    — per-source run-creation and per-run tool-call token
//     buckets.
//
// All files share the runtime package; the split is purely to keep each
// unit small enough to navigate without grep.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/llm"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/metrics"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// Runtime orchestrates runs, steps, and tool execution.
type Runtime struct {
	db             *db.DB
	runRepo        *db.RunRepository
	stepRepo       *db.StepRepository
	planRepo       *db.PlanRepository
	assistantRepo  *db.AssistantRepository
	settingsRepo   *db.AppSettingsRepository
	attachmentRepo *db.RunAttachmentRepository
	enrichment     *EnrichmentService
	eventBus       *events.EventBus
	permEngine     *permissions.Engine
	approvalMgr    *permissions.Manager
	toolExecutor   *tools.Executor
	memManager     *memory.Manager
	maxRetries     int

	// rootCtx is the parent context for every background run. Shutdown()
	// cancels it, which propagates into in-flight tool executions
	// (including command.exec's 30s timeout) and the approval-wait paths.
	// Without it a SIGTERM would wait on the HTTP server while leaving
	// every run's goroutine running to completion.
	rootCtx    context.Context
	rootCancel context.CancelFunc

	// connectorManifest resolves a connector source name to its declared
	// capability permissions. When a run is sourced from a connector, the
	// runtime intersects the assistant's policy with these permissions so
	// an untrusted plugin cannot exercise any capability it didn't declare.
	// A nil lookup is treated as "source unknown" and all connector-sourced
	// runs are denied (secure default).
	connectorManifest ConnectorManifestLookup

	// limiter applies per-source run-creation and per-run tool-call token
	// buckets to blunt agent-loop pathologies and connector flooding.
	limiter *rateLimiter

	// llmResolver produces LLM clients for the llm.chat tool and for
	// future planning-side LLM calls. May be nil — then planSteps falls
	// back to the legacy command.exec behavior so the daemon still boots
	// and runs without any provider configured.
	llmResolver *llm.Resolver

	// planApprovals signals runs waiting in plan_review so they can continue
	// immediately when ApprovePlan transitions the run to executing.
	planMu        sync.RWMutex
	planApprovals map[string]chan struct{}

	// pauseSignals wakes paused run goroutines when ResumeRun is called.
	pauseMu      sync.RWMutex
	pauseSignals map[string]chan struct{}

	// replanCounts tracks how many automatic replans each run has
	// triggered. Capped at MaxReplansPerRun so a planner that keeps
	// emitting bad plans can't burn the user's token budget. In-memory
	// only — replans across daemon restarts are rare enough that
	// persisting the counter isn't worth a migration.
	replanMu     sync.Mutex
	replanCounts map[string]int
}

// MaxReplansPerRun bounds the automatic replan loop. After hitting this
// ceiling, the run fails normally so a human can intervene. The number
// is small on purpose: replans are expensive and a planner that can't
// fix a problem in two tries is unlikely to fix it in twenty.
const MaxReplansPerRun = 2

// ConnectorManifestLookup returns the capability allowlist declared in the
// named connector's manifest. ok=false indicates the connector isn't
// registered, in which case the runtime denies every capability for runs
// sourced from that name.
type ConnectorManifestLookup func(name string) (capabilities []string, ok bool)

// SetConnectorManifestLookup installs the manifest resolver. Called from
// main() after the connector registry is populated so the runtime can enforce
// manifest ∩ assistant-policy at tool-execution time.
func (r *Runtime) SetConnectorManifestLookup(fn ConnectorManifestLookup) {
	r.connectorManifest = fn
}

// SetLLMResolver installs the LLM resolver used by planSteps to decide
// whether the runtime has a real "brain" configured. When set and a default
// provider is available, planSteps produces steps that route to llm.chat
// rather than the legacy command.exec shortcut.
func (r *Runtime) SetLLMResolver(resolver *llm.Resolver) {
	r.llmResolver = resolver
}

// hasDefaultLLM reports whether a default LLM provider has been configured
// AND can be resolved. Returns false (without error) when the user simply
// hasn't set up a provider yet — that's a normal state, not a failure.
func (r *Runtime) hasDefaultLLM() bool {
	if r.llmResolver == nil {
		return false
	}
	client, _, err := r.llmResolver.DefaultClient()
	if err != nil {
		// Configured but broken: log once per run rather than silently
		// falling back. Observable via the events log as future work.
		return false
	}
	return client != nil
}

// Config holds runtime configuration.
type Config struct {
	MaxRetries int

	// RunsPerMinutePerSource caps how many runs a single source (connector
	// name) can create per minute. Prevents a misbehaving Telegram bot or a
	// malicious plugin from flooding the daemon. Desktop-sourced runs
	// (source == nil) are not rate-limited.
	RunsPerMinutePerSource int
	RunsBurst              int

	// ToolCallsPerMinutePerRun caps how many tool invocations a single run
	// can perform per minute. Agent loops that oscillate between plan-retry
	// and execution would otherwise keep submitting commands; exceeding the
	// limit fails the step with a clear error.
	ToolCallsPerMinutePerRun int
	ToolCallsBurst           int
}

// DefaultConfig returns default runtime configuration.
func DefaultConfig() Config {
	return Config{
		MaxRetries:               3,
		RunsPerMinutePerSource:   30,
		RunsBurst:                5,
		ToolCallsPerMinutePerRun: 120,
		ToolCallsBurst:           10,
	}
}

// NewRuntime creates a new runtime instance.
func NewRuntime(
	database *db.DB,
	eventBus *events.EventBus,
	permEngine *permissions.Engine,
	approvalMgr *permissions.Manager,
	toolExecutor *tools.Executor,
	memManager *memory.Manager,
	config Config,
) *Runtime {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	attachmentRepo := db.NewRunAttachmentRepository(database)
	rt := &Runtime{
		db:             database,
		runRepo:        db.NewRunRepository(database),
		stepRepo:       db.NewStepRepository(database),
		planRepo:       db.NewPlanRepository(database),
		assistantRepo:  db.NewAssistantRepository(database),
		settingsRepo:   db.NewAppSettingsRepository(database),
		attachmentRepo: attachmentRepo,
		eventBus:       eventBus,
		permEngine:     permEngine,
		approvalMgr:    approvalMgr,
		toolExecutor:   toolExecutor,
		memManager:     memManager,
		maxRetries:     config.MaxRetries,
		rootCtx:        rootCtx,
		rootCancel:     rootCancel,
		planApprovals:  make(map[string]chan struct{}),
		pauseSignals:   make(map[string]chan struct{}),
		replanCounts:   make(map[string]int),
		limiter: newRateLimiter(
			config.RunsPerMinutePerSource, config.RunsBurst,
			config.ToolCallsPerMinutePerRun, config.ToolCallsBurst,
		),
	}
	// Enrichment service uses the same toolExecutor so it can call
	// media.transcribe / media.describe_image without a separate
	// dispatch path. Bound after the struct so it can capture the
	// already-built attachmentRepo + executor.
	rt.enrichment = NewEnrichmentService(attachmentRepo, toolExecutor, nil)
	return rt
}

// Shutdown cancels the runtime root context so in-flight runs, tool
// executions, and approval waits abort promptly. Callers should invoke this
// during graceful shutdown before Close-ing the HTTP server so goroutines
// have a chance to unwind.
func (r *Runtime) Shutdown() {
	if r.rootCancel != nil {
		r.rootCancel()
	}
}

// ResumeOrphanedRuns is called at daemon startup to find runs that were
// mid-execution when the previous process died and re-attach a goroutine
// to each. Without this, any run in planning, plan_review, executing,
// awaiting_approval, or paused would be stuck forever after a restart
// because the only thing driving state transitions is the per-run goroutine.
//
// Approval-waiting runs remain in awaiting_approval; the existing
// approvalMgr path takes over when the user resolves the approval via the
// UI. All other non-terminal runs re-enter executeRun so they re-plan/
// re-execute the latest plan version.
func (r *Runtime) ResumeOrphanedRuns() error {
	nonTerminal := []domain.RunStatus{
		domain.RunPlanning,
		domain.RunPlanReview,
		domain.RunExecuting,
		domain.RunAwaitingApproval,
		domain.RunPaused,
	}
	runs, err := r.runRepo.ListByStatusIn(nonTerminal)
	if err != nil {
		return fmt.Errorf("failed to list orphaned runs: %w", err)
	}

	for _, run := range runs {
		// Awaiting-approval runs don't need a resumer goroutine; the
		// approval callback will re-enter the execution path. For the
		// others, we restart the run from its current state.
		if run.Status == domain.RunAwaitingApproval {
			continue
		}

		assistant, err := r.assistantRepo.GetByID(run.AssistantID)
		if err != nil {
			// Assistant was deleted while the run was in flight. Fail the
			// run so the UI shows a clear terminal state instead of leaving
			// it hanging.
			_ = r.transitionRun(r.rootCtx, run, domain.RunFailed)
			_, _ = r.eventBus.Publish(r.rootCtx, domain.EventRunFailed, run.ID, nil, map[string]interface{}{
				"error": "assistant deleted before run resumed",
			})
			continue
		}

		// Executing/paused runs need to go back through the planning
		// entry point. The planning phase is idempotent (plan_version
		// increments on retry); the runtime will create a new plan and
		// pick up from there.
		runCopy := *run
		go r.executeRun(r.rootCtx, &runCopy, assistant)
	}
	return nil
}

// CreateRun creates a new run from the desktop UI / REST API. The run's
// Source is left nil, which the runtime treats as "trusted local user" for
// permission evaluation.
func (r *Runtime) CreateRun(ctx context.Context, goal, assistantID string) (*domain.Run, error) {
	return r.createRun(ctx, goal, assistantID, nil, "")
}

// CreateRunFromSource creates a new run initiated by a connector. The
// connector name is recorded on the run and intersected with the assistant's
// permission policy at tool-execution time.
func (r *Runtime) CreateRunFromSource(ctx context.Context, goal, assistantID, source string) (*domain.Run, error) {
	if source == "" {
		return r.createRun(ctx, goal, assistantID, nil, "")
	}
	return r.createRun(ctx, goal, assistantID, &source, "")
}

// AttachToRun records inbound media metadata for an existing run.
// Channel plugins call this immediately after CreateRunInConversation
// when their inbound message carries non-text content. Idempotent at
// the per-attachment level — duplicate IDs are rejected by the repo.
//
// Bytes are NOT stored; the attachment carries a URL or external_id
// the future enrichment pass uses to fetch on demand. Keeps SQLite
// small and avoids the daemon doubling as a media store.
func (r *Runtime) AttachToRun(runID string, attachments []*domain.RunAttachment) error {
	if r.attachmentRepo == nil || len(attachments) == 0 {
		return nil
	}
	for _, a := range attachments {
		a.RunID = runID
	}
	return r.attachmentRepo.CreateBatch(attachments)
}

// ListRunAttachments returns the captured attachments for a run, in
// arrival order. Used by the (forthcoming) enrichment pass and by
// the UI to render attachment chips on each run.
func (r *Runtime) ListRunAttachments(runID string) ([]*domain.RunAttachment, error) {
	if r.attachmentRepo == nil {
		return nil, nil
	}
	return r.attachmentRepo.ListByRun(runID)
}

// CreateRunInConversation creates a new run initiated by a channel plugin
// and links it to a persistent Conversation thread. Used by channel
// plugins (Telegram, Email, Slack, Discord) so the Chats UI can group
// multi-turn interactions under one thread. Source is required; an empty
// conversationID falls back to the legacy per-run behavior.
func (r *Runtime) CreateRunInConversation(ctx context.Context, goal, assistantID, source, conversationID string) (*domain.Run, error) {
	if source == "" {
		return r.createRun(ctx, goal, assistantID, nil, conversationID)
	}
	return r.createRun(ctx, goal, assistantID, &source, conversationID)
}

func (r *Runtime) createRun(ctx context.Context, goal, assistantID string, source *string, conversationID string) (*domain.Run, error) {
	// Rate-limit connector-sourced run creation. Desktop runs are trusted
	// local user intent and bypass the limiter.
	if source != nil && *source != "" {
		if !r.limiter.AllowRun(*source) {
			return nil, fmt.Errorf("run creation rate limit exceeded for source %q", *source)
		}
	}

	assistant, err := r.assistantRepo.GetByID(assistantID)
	if err != nil {
		return nil, fmt.Errorf("assistant not found: %w", err)
	}

	run := &domain.Run{
		ID:          uuid.New().String(),
		Goal:        goal,
		AssistantID: assistantID,
		Source:      source,
		Status:      domain.RunCreated,
		PlanVersion: 1,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if conversationID != "" {
		cid := conversationID
		run.ConversationID = &cid
	}

	if err := r.runRepo.Create(run); err != nil {
		return nil, fmt.Errorf("failed to create run: %w", err)
	}

	metrics.RunsCreatedTotal.Inc()
	slog.Info("run created", "run_id", run.ID, "assistant_id", assistantID, "goal", goal, "source", source)
	payload := map[string]interface{}{
		"goal":         goal,
		"assistant_id": assistantID,
	}
	if source != nil {
		payload["source"] = *source
	}
	_, err = r.eventBus.Publish(ctx, domain.EventRunCreated, run.ID, nil, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to publish run created event: %w", err)
	}

	// Start execution asynchronously on an owned copy of the run. The
	// caller receives its own pointer and can read run.Status without
	// racing the background goroutine that mutates state through the
	// lifecycle.
	runCopy := *run
	go r.executeRun(r.rootCtx, &runCopy, assistant)

	return run, nil
}

// GetRun retrieves a run by ID with its steps and plan.
func (r *Runtime) GetRun(id string) (*domain.Run, []*domain.Step, *domain.Plan, error) {
	run, err := r.runRepo.GetByID(id)
	if err != nil {
		return nil, nil, nil, err
	}

	steps, err := r.stepRepo.ListByRun(id)
	if err != nil {
		return nil, nil, nil, err
	}

	plan, err := r.planRepo.GetByRunID(id)
	if err != nil {
		// Plan is optional, don't fail if not found
		plan = nil
	}

	return run, steps, plan, nil
}

// ListRuns retrieves all runs.
func (r *Runtime) ListRuns() ([]*domain.Run, error) {
	return r.runRepo.List(nil, 100, 0)
}

// GetRunApprovals retrieves all approvals for a run.
func (r *Runtime) GetRunApprovals(runID string) ([]*permissions.ApprovalRequest, error) {
	return r.approvalMgr.GetByRun(runID)
}

// ApproveRun approves a pending run by resolving all pending approvals for it.
func (r *Runtime) ApproveRun(ctx context.Context, runID string) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if run.Status != domain.RunAwaitingApproval {
		return fmt.Errorf("run is not awaiting approval: %s", run.Status)
	}

	// Resolve all pending approvals for this run
	approvals, err := r.approvalMgr.GetByRun(runID)
	if err != nil {
		return fmt.Errorf("failed to get approvals for run: %w", err)
	}

	resolved := false
	for _, approval := range approvals {
		if approval.Status == permissions.ApprovalPending {
			if err := r.approvalMgr.Resolve(ctx, approval.ID, true); err != nil {
				return fmt.Errorf("failed to resolve approval %s: %w", approval.ID, err)
			}
			resolved = true
		}
	}

	if !resolved {
		return fmt.Errorf("no pending approvals found for run")
	}

	// State transition will happen when the blocked goroutine receives the signal
	// and resumes execution, transitioning back to executing
	return nil
}

// ApprovePlan approves the proposed plan for a run.
// Transitions the run from plan_review to executing.
// The original executeRun goroutine will detect the status change and continue execution.
func (r *Runtime) ApprovePlan(ctx context.Context, runID string) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if run.Status != domain.RunPlanReview {
		return fmt.Errorf("run is not awaiting plan review: %s", run.Status)
	}

	// Transition to executing — the polling goroutine will pick this up
	if err := r.transitionRun(ctx, run, domain.RunExecuting); err != nil {
		return fmt.Errorf("failed to transition run to executing: %w", err)
	}
	r.signalPlanApproval(runID)

	return nil
}

// EditPlan updates the plan for a run with new step definitions.
// The run must be in plan_review status. This creates a new plan version.
func (r *Runtime) EditPlan(ctx context.Context, runID string, stepDefs []domain.StepDefinition) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if run.Status != domain.RunPlanReview {
		return fmt.Errorf("run is not in plan review: %s", run.Status)
	}

	oldPlan, _ := r.planRepo.GetByRunID(runID)

	// Delete existing steps for this run
	existingSteps, err := r.stepRepo.ListByRun(runID)
	if err != nil {
		return fmt.Errorf("failed to list existing steps: %w", err)
	}
	for _, step := range existingSteps {
		if err := r.stepRepo.Delete(step.ID); err != nil {
			return fmt.Errorf("failed to delete step: %w", err)
		}
	}

	// Get next plan version
	version, err := r.planRepo.GetPlanVersion(runID)
	if err != nil {
		version = 1
	}

	// Create new plan
	plan := &domain.Plan{
		ID:        uuid.New().String(),
		RunID:     runID,
		Version:   version,
		CreatedAt: time.Now().UTC(),
	}

	// Build input with context
	assistant, err := r.assistantRepo.GetByID(run.AssistantID)
	if err != nil {
		return fmt.Errorf("assistant not found: %w", err)
	}
	contextData, _ := r.loadFolderContexts(ctx, assistant)
	input := run.Goal
	if contextData != "" {
		input = fmt.Sprintf("%s\n\nAttached context:\n%s", run.Goal, contextData)
	}

	// Create steps from edited definitions
	idMap := make(map[string]string, len(stepDefs))
	steps := make([]*domain.Step, 0, len(stepDefs))
	for i, def := range stepDefs {
		oldID := def.ID
		def.ID = uuid.New().String()
		if oldID != "" {
			idMap[oldID] = def.ID
		}
		def.PlanID = plan.ID
		def.Order = i
		def.CreatedAt = time.Now().UTC()
		plan.Steps = append(plan.Steps, def)

		stepDefID := def.ID
		step := &domain.Step{
			ID:               uuid.New().String(),
			RunID:            runID,
			StepDefinitionID: &stepDefID,
			Title:            def.Title,
			Status:           domain.StepPending,
			Input:            input,
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
		}
		steps = append(steps, step)
	}

	// Remap dependencies from request IDs to freshly generated IDs.
	for i := range plan.Steps {
		deps := make([]string, 0, len(plan.Steps[i].DependsOn))
		for _, dep := range plan.Steps[i].DependsOn {
			if mapped, ok := idMap[dep]; ok {
				deps = append(deps, mapped)
			}
		}
		plan.Steps[i].DependsOn = deps
	}

	// Save plan
	if err := r.planRepo.Create(plan); err != nil {
		return fmt.Errorf("failed to create plan: %w", err)
	}

	// Save steps
	for _, step := range steps {
		if err := r.stepRepo.Create(step); err != nil {
			return fmt.Errorf("failed to create step: %w", err)
		}
	}

	// Update run plan version
	run.PlanVersion = plan.Version
	if err := r.runRepo.Update(run); err != nil {
		return fmt.Errorf("failed to update run: %w", err)
	}

	// Publish event
	_, _ = r.eventBus.Publish(ctx, domain.EventPlanProposed, runID, nil, map[string]interface{}{
		"plan_id":      plan.ID,
		"plan_version": plan.Version,
		"step_count":   len(plan.Steps),
		"edited":       true,
	})

	// Edit-distance metrics: count titles added vs removed vs
	// replaced so dashboards can spot a planner whose proposals are
	// being heavily rewritten. provider label is best-effort — the
	// EditPlan path doesn't have the client in scope, so we resolve
	// it lazily here.
	if oldPlan != nil {
		var editClient llm.Client
		if r.llmResolver != nil {
			editClient, _, _ = r.llmResolver.DefaultClient()
		}
		emitPlannerEditDistance(plannerProviderLabel(editClient), oldPlan.Steps, plan.Steps)
	}

	// Capture plan-edit preference memory so future planning can adapt.
	if oldPlan != nil && r.memManager != nil {
		assistantID := run.AssistantID
		now := time.Now().UTC()

		// Build map of old steps by title for comparison
		oldStepTitles := make(map[string]domain.StepDefinition)
		for _, s := range oldPlan.Steps {
			oldStepTitles[s.Title] = s
		}

		// Detect removed steps (rejections)
		newStepTitles := make(map[string]bool)
		for _, s := range plan.Steps {
			newStepTitles[s.Title] = true
		}
		for _, oldStep := range oldPlan.Steps {
			if !newStepTitles[oldStep.Title] {
				entry := &domain.MemoryEntry{
					Scope:       "preferences",
					Content:     fmt.Sprintf("User removed step: '%s' (%s). Avoid similar steps for similar goals.", oldStep.Title, oldStep.Description),
					AssistantID: &assistantID,
					RunID:       &run.ID,
					CreatedAt:   now,
				}
				_ = r.memManager.Save(entry)
			}
		}

		// Generic edit summary
		entry := &domain.MemoryEntry{
			Scope:       "preferences",
			Content:     fmt.Sprintf("User edited plan for run %s (from %d steps to %d). Prefer this revised structure for similar goals.", run.ID, len(oldPlan.Steps), len(plan.Steps)),
			AssistantID: &assistantID,
			RunID:       &run.ID,
			CreatedAt:   now,
		}
		_ = r.memManager.Save(entry)
	}

	return nil
}

// Replan asks the planner to produce a new plan for the running run,
// seeded with the previously executed steps' outputs and the failure
// reason. Used both as the automatic recovery path when a step fails
// (the executeExecutionPhase loop calls this before falling back to
// failRun) and as a user-driven "Fix this with the agent" CTA on a
// failed run. The replan budget is bounded by MaxReplansPerRun so a
// planner that can't fix the problem doesn't burn the user's tokens.
//
// Returns the fresh step list on success. Caller is expected to swap
// the run's in-memory step slice and rewind its loop index.
func (r *Runtime) Replan(
	ctx context.Context,
	run *domain.Run,
	failedStep *domain.Step,
	failureMessage string,
) ([]*domain.Step, error) {
	// Resolve the provider client up front so metric labels can name
	// the actual backend. nil → "unknown" (acceptable: budget-error
	// path doesn't need provider granularity).
	var client llm.Client
	if r.llmResolver != nil {
		client, _, _ = r.llmResolver.DefaultClient()
	}
	provider := plannerProviderLabel(client)

	r.replanMu.Lock()
	count := r.replanCounts[run.ID]
	if count >= MaxReplansPerRun {
		r.replanMu.Unlock()
		metrics.PlannerCallsTotal.WithLabelValues(provider, "replan_max_exceeded").Inc()
		return nil, fmt.Errorf("replan budget exhausted (max %d): leaving run in failed state for human follow-up", MaxReplansPerRun)
	}
	r.replanCounts[run.ID] = count + 1
	r.replanMu.Unlock()

	if !r.hasDefaultLLM() {
		return nil, fmt.Errorf("replan requires a configured default LLM")
	}
	assistant, err := r.assistantRepo.GetByID(run.AssistantID)
	if err != nil {
		return nil, fmt.Errorf("replan: assistant lookup: %w", err)
	}

	// Build the previous-attempts blob from the run's existing step
	// outputs. We pass the failure message verbatim so the LLM sees
	// stderr / tool error / approval-denied reason. Wrapped in a
	// trusted=false tag downstream by the planner.
	priorSteps, err := r.stepRepo.ListByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("replan: list prior steps: %w", err)
	}
	previous := summarizePriorAttempts(priorSteps, failedStep, failureMessage)

	contextData, _ := r.loadFolderContexts(ctx, assistant)
	planner := r.planWithLLMOpts(ctx, run.Goal, assistant, contextData, previous)
	if len(planner) == 0 {
		metrics.PlannerCallsTotal.WithLabelValues(provider, "replan_empty").Inc()
		return nil, fmt.Errorf("replan: planner returned no usable steps")
	}

	// Replace the run's plan in the same way EditPlan does: increment
	// version, rewrite step rows, persist.
	stepDefs := make([]domain.StepDefinition, len(planner))
	for i, ps := range planner {
		stepDefs[i] = domain.StepDefinition{
			ID:                 uuid.New().String(),
			Title:              ps.Title,
			Description:        ps.Description,
			ExpectedTool:       ps.Tool,
			ExpectedCapability: r.getCapabilityForTool(ps.Tool),
			Arguments:          ps.Arguments,
			Order:              i,
		}
	}

	existing, err := r.stepRepo.ListByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("replan: list existing steps: %w", err)
	}
	for _, s := range existing {
		if err := r.stepRepo.Delete(s.ID); err != nil {
			return nil, fmt.Errorf("replan: delete prior step: %w", err)
		}
	}

	version, err := r.planRepo.GetPlanVersion(run.ID)
	if err != nil {
		version = 1
	}
	planID := uuid.New().String()
	for i := range stepDefs {
		stepDefs[i].PlanID = planID
		stepDefs[i].CreatedAt = time.Now().UTC()
	}
	plan := &domain.Plan{
		ID:        planID,
		RunID:     run.ID,
		Version:   version,
		Steps:     stepDefs,
		CreatedAt: time.Now().UTC(),
	}
	if err := r.planRepo.Create(plan); err != nil {
		return nil, fmt.Errorf("replan: create plan: %w", err)
	}

	newSteps := make([]*domain.Step, 0, len(stepDefs))
	for i, def := range stepDefs {
		stepID := uuid.New().String()
		stepCopy := def
		step := &domain.Step{
			ID:               stepID,
			RunID:            run.ID,
			StepDefinitionID: &stepCopy.ID,
			Title:            def.Title,
			Input:            run.Goal,
			Status:           domain.StepPending,
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
		}
		if i == 0 {
			step.Status = domain.StepReady
		}
		if err := r.stepRepo.Create(step); err != nil {
			return nil, fmt.Errorf("replan: create step: %w", err)
		}
		newSteps = append(newSteps, step)
	}

	run.PlanVersion = plan.Version
	run.UpdatedAt = time.Now().UTC()
	if err := r.runRepo.Update(run); err != nil {
		return nil, fmt.Errorf("replan: update run: %w", err)
	}

	_, _ = r.eventBus.Publish(ctx, domain.EventPlanProposed, run.ID, nil, map[string]interface{}{
		"plan_id":      plan.ID,
		"plan_version": plan.Version,
		"step_count":   len(plan.Steps),
		"replan":       true,
		"replan_count": count + 1,
		"failure":      failureMessage,
	})

	metrics.PlannerCallsTotal.WithLabelValues(provider, "replan_ok").Inc()
	return newSteps, nil
}

// ManualReplan is the user-driven counterpart to the automatic
// replan path: the run is already terminal (failed) and the user
// clicked "Fix this with the agent". We find the most recent failed
// step, read the failure, then delegate to Replan and re-launch the
// executor goroutine so the new plan actually runs. Bounded by the
// same MaxReplansPerRun budget.
func (r *Runtime) ManualReplan(ctx context.Context, runID string) ([]*domain.Step, error) {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return nil, err
	}
	if !run.Status.IsTerminal() {
		return nil, fmt.Errorf("manual replan only valid on terminal runs, got %s", run.Status)
	}
	priorSteps, err := r.stepRepo.ListByRun(runID)
	if err != nil {
		return nil, err
	}
	var failedStep *domain.Step
	failureMessage := "previous run did not complete successfully"
	for _, s := range priorSteps {
		if s.Status == domain.StepFailed {
			failedStep = s
			if s.Error != nil {
				failureMessage = *s.Error
			}
		}
	}
	newSteps, err := r.Replan(ctx, run, failedStep, failureMessage)
	if err != nil {
		return nil, err
	}

	// Move the run back into the active path. Terminal → created is
	// already a legal edge (see RetryRun). The executor will pick up
	// the new step rows and re-execute.
	if err := r.transitionRun(ctx, run, domain.RunCreated); err != nil {
		return nil, fmt.Errorf("manual replan transition: %w", err)
	}
	assistant, err := r.assistantRepo.GetByID(run.AssistantID)
	if err != nil {
		return nil, err
	}
	go r.executeRun(r.rootCtx, run, assistant)
	return newSteps, nil
}

// RetryRun retries a terminal run by resetting it to Created and starting a
// fresh executeRun goroutine. The terminal→Created state-machine edges (#15)
// make this a legal transition rather than a bypass.
func (r *Runtime) RetryRun(ctx context.Context, runID string) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if !run.Status.IsTerminal() {
		return fmt.Errorf("run is not in a terminal state: %s", run.Status)
	}

	if err := r.transitionRun(ctx, run, domain.RunCreated); err != nil {
		return fmt.Errorf("failed to transition run to created for retry: %w", err)
	}
	run.PlanVersion++
	run.UpdatedAt = time.Now().UTC()
	if err := r.runRepo.Update(run); err != nil {
		return err
	}

	assistant, err := r.assistantRepo.GetByID(run.AssistantID)
	if err != nil {
		return err
	}

	go r.executeRun(r.rootCtx, run, assistant)
	return nil
}

// ForkRun creates a child run branched from a parent run at a specific step.
// The child inherits the parent's assistant and starts with a goal derived from
// the step's title (or the provided override goal). The parent run continues
// independently.
func (r *Runtime) ForkRun(ctx context.Context, parentID, stepID, goalOverride string) (*domain.Run, error) {
	parent, err := r.runRepo.GetByID(parentID)
	if err != nil {
		return nil, fmt.Errorf("parent run not found: %w", err)
	}

	// Find the step to branch from
	steps, err := r.stepRepo.ListByRun(parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to list parent steps: %w", err)
	}
	var branchStep *domain.Step
	for _, s := range steps {
		if s.ID == stepID || (s.StepDefinitionID != nil && *s.StepDefinitionID == stepID) {
			branchStep = s
			break
		}
	}
	if branchStep == nil {
		return nil, fmt.Errorf("step %s not found in parent run", stepID)
	}

	goal := goalOverride
	if goal == "" {
		goal = fmt.Sprintf("(branched from %s) %s", parentID[:8], branchStep.Title)
	}

	child := &domain.Run{
		ID:                 uuid.New().String(),
		Goal:               goal,
		AssistantID:        parent.AssistantID,
		Status:             domain.RunCreated,
		PlanVersion:        1,
		RunParentID:        &parent.ID,
		BranchedFromStepID: &stepID,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if parent.Source != nil {
		child.Source = parent.Source
	}

	if err := r.runRepo.Create(child); err != nil {
		return nil, fmt.Errorf("failed to create child run: %w", err)
	}

	_, _ = r.eventBus.Publish(ctx, domain.EventRunCreated, child.ID, nil, map[string]interface{}{
		"goal":                  child.Goal,
		"assistant_id":          child.AssistantID,
		"parent_id":             parent.ID,
		"branched_from_step_id": stepID,
	})

	assistant, err := r.assistantRepo.GetByID(child.AssistantID)
	if err != nil {
		return nil, err
	}

	go r.executeRun(r.rootCtx, child, assistant)
	return child, nil
}

// PauseRun transitions an active run to paused.
func (r *Runtime) PauseRun(ctx context.Context, runID string) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if run.Status != domain.RunExecuting && run.Status != domain.RunAwaitingApproval {
		return fmt.Errorf("run is not pausable: %s", run.Status)
	}
	from := run.Status

	if err := r.transitionRun(ctx, run, domain.RunPaused); err != nil {
		return fmt.Errorf("failed to pause run: %w", err)
	}

	_, _ = r.eventBus.Publish(ctx, domain.EventRunPaused, run.ID, nil, map[string]interface{}{
		"from_status": string(from),
	})
	return nil
}

// ResumeRun transitions a paused run back to executing and signals any waiter.
func (r *Runtime) ResumeRun(ctx context.Context, runID string) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if run.Status != domain.RunPaused {
		return fmt.Errorf("run is not paused: %s", run.Status)
	}

	if err := r.transitionRun(ctx, run, domain.RunExecuting); err != nil {
		return fmt.Errorf("failed to resume run: %w", err)
	}

	r.signalPauseResume(runID)
	_, _ = r.eventBus.Publish(ctx, domain.EventRunResumed, run.ID, nil, map[string]interface{}{})
	return nil
}

// CancelRun transitions an active run to cancelled.
func (r *Runtime) CancelRun(ctx context.Context, runID string) error {
	run, err := r.runRepo.GetByID(runID)
	if err != nil {
		return err
	}

	if run.Status.IsTerminal() {
		return fmt.Errorf("run is already terminal: %s", run.Status)
	}

	from := run.Status
	if err := r.transitionRun(ctx, run, domain.RunCancelled); err != nil {
		return fmt.Errorf("failed to cancel run: %w", err)
	}

	_, _ = r.eventBus.Publish(ctx, domain.EventRunCancelled, run.ID, nil, map[string]interface{}{
		"from_status": string(from),
	})
	r.limiter.ForgetRun(run.ID)
	return nil
}

// DeleteRun deletes a run and its associated data.
func (r *Runtime) DeleteRun(runID string) error {
	r.limiter.ForgetRun(runID)
	return r.runRepo.Delete(runID)
}

// strPtr is a helper to create a string pointer; used across lifecycle files
// to set Step.Error without repeating the two-line address-of dance.
func strPtr(s string) *string {
	return &s
}

func (r *Runtime) registerPauseSignal(runID string) chan struct{} {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if ch, ok := r.pauseSignals[runID]; ok {
		return ch
	}
	ch := make(chan struct{}, 1)
	r.pauseSignals[runID] = ch
	return ch
}

func (r *Runtime) unregisterPauseSignal(runID string, ch chan struct{}) {
	r.pauseMu.Lock()
	defer r.pauseMu.Unlock()
	if cur, ok := r.pauseSignals[runID]; ok && cur == ch {
		delete(r.pauseSignals, runID)
	}
}

func (r *Runtime) signalPauseResume(runID string) {
	r.pauseMu.RLock()
	ch, ok := r.pauseSignals[runID]
	r.pauseMu.RUnlock()
	if !ok {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *Runtime) registerPlanApproval(runID string) chan struct{} {
	r.planMu.Lock()
	defer r.planMu.Unlock()
	ch := make(chan struct{}, 1)
	r.planApprovals[runID] = ch
	return ch
}

func (r *Runtime) unregisterPlanApproval(runID string, ch chan struct{}) {
	r.planMu.Lock()
	defer r.planMu.Unlock()
	if cur, ok := r.planApprovals[runID]; ok && cur == ch {
		delete(r.planApprovals, runID)
	}
}

func (r *Runtime) signalPlanApproval(runID string) {
	r.planMu.RLock()
	ch, ok := r.planApprovals[runID]
	r.planMu.RUnlock()
	if !ok {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}
