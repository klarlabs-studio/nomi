package domain

import (
	"time"
)

// RunStatus represents the possible states of a Run
type RunStatus string

const (
	RunCreated          RunStatus = "created"
	RunPlanning         RunStatus = "planning"
	RunPlanReview       RunStatus = "plan_review"
	RunAwaitingApproval RunStatus = "awaiting_approval"
	RunExecuting        RunStatus = "executing"
	RunPaused           RunStatus = "paused"
	RunCompleted        RunStatus = "completed"
	RunFailed           RunStatus = "failed"
	RunCancelled        RunStatus = "cancelled"
)

// IsValid checks if the run status is valid
func (s RunStatus) IsValid() bool {
	switch s {
	case RunCreated, RunPlanning, RunPlanReview, RunAwaitingApproval, RunExecuting,
		RunPaused, RunCompleted, RunFailed, RunCancelled:
		return true
	}
	return false
}

// IsTerminal returns true if the run has reached a terminal state
func (s RunStatus) IsTerminal() bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled
}

// Run represents an execution of an assistant.
//
// Source identifies where the run was initiated:
//   - "" / nil / "desktop" — the local user via the Tauri UI or REST API
//   - "<connector_name>"   — a connector like "telegram"
//
// The runtime enforces that a connector-sourced run can only exercise tools
// whose capabilities are listed in that connector's manifest, regardless of
// what the assistant's permission policy would otherwise allow.
type Run struct {
	ID          string  `json:"id"`
	Goal        string  `json:"goal"`
	AssistantID string  `json:"assistant_id"`
	Source      *string `json:"source,omitempty"`
	// ConversationID links channel-originated runs to their persistent
	// thread (ADR 0001 §8). Nil for desktop-initiated runs that don't
	// belong to a channel thread.
	ConversationID *string   `json:"conversation_id,omitempty"`
	Status         RunStatus `json:"status"`
	CurrentStepID  *string   `json:"current_step_id,omitempty"`
	PlanVersion    int       `json:"plan_version"`
	// RunParentID identifies the run this one was branched from.
	// Nil for top-level runs.
	RunParentID *string `json:"run_parent_id,omitempty"`
	// BranchedFromStepID identifies which step in the parent run
	// this child run was forked from. Nil for top-level runs.
	BranchedFromStepID *string   `json:"branched_from_step_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// StepStatus represents the possible states of a Step
type StepStatus string

const (
	StepPending  StepStatus = "pending"
	StepReady    StepStatus = "ready"
	StepRunning  StepStatus = "running"
	StepRetrying StepStatus = "retrying"
	StepBlocked  StepStatus = "blocked"
	StepDone     StepStatus = "done"
	StepFailed   StepStatus = "failed"
)

// IsValid checks if the step status is valid
func (s StepStatus) IsValid() bool {
	switch s {
	case StepPending, StepReady, StepRunning, StepRetrying,
		StepBlocked, StepDone, StepFailed:
		return true
	}
	return false
}

// IsTerminal returns true if the step has reached a terminal state
func (s StepStatus) IsTerminal() bool {
	return s == StepDone || s == StepFailed
}

// Step represents a single unit of work within a Run
type Step struct {
	ID               string     `json:"id"`
	RunID            string     `json:"run_id"`
	StepDefinitionID *string    `json:"step_definition_id,omitempty"`
	Title            string     `json:"title"`
	Status           StepStatus `json:"status"`
	Input            string     `json:"input,omitempty"`
	Output           string     `json:"output,omitempty"`
	Error            *string    `json:"error,omitempty"`
	RetryCount       int        `json:"retry_count"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// StepDefinition represents a planned step in a Plan
type StepDefinition struct {
	ID                 string `json:"id"`
	PlanID             string `json:"plan_id"`
	Title              string `json:"title"`
	Description        string `json:"description,omitempty"`
	ExpectedTool       string `json:"expected_tool,omitempty"`
	ExpectedCapability string `json:"expected_capability,omitempty"`
	Why                string `json:"why,omitempty"` // Why this step was planned (e.g., "Based on your preference for...")
	// Arguments is a tool-specific keyed payload the planner emits so the
	// runtime can call structured tools (filesystem.write needs path+content,
	// command.exec needs command, etc.) without the planner having to
	// encode every tool's contract into a free-form string. Empty for
	// fallback single-step plans and for tools that take no arguments
	// beyond the goal text (e.g. llm.chat).
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	Order     int                    `json:"order"`
	// DependsOn lists the IDs of other StepDefinitions in the same Plan
	// that must complete before this step can start. Empty means the step
	// has no prerequisites. Used to render the plan as a DAG.
	DependsOn []string  `json:"depends_on"`
	CreatedAt time.Time `json:"created_at"`
}

// Plan represents a proposed execution plan for a Run
type Plan struct {
	ID        string           `json:"id"`
	RunID     string           `json:"run_id"`
	Version   int              `json:"version"`
	Steps     []StepDefinition `json:"steps"`
	CreatedAt time.Time        `json:"created_at"`
}

// ContextAttachment represents attached context (folder, repo, etc.)
type ContextAttachment struct {
	Type string `json:"type"` // "folder", "repo", etc.
	Path string `json:"path"`
}

// PermissionMode represents the evaluation result of a permission rule
type PermissionMode string

const (
	PermissionAllow   PermissionMode = "allow"
	PermissionConfirm PermissionMode = "confirm"
	PermissionDeny    PermissionMode = "deny"
)

// IsValid checks if the permission mode is valid
func (m PermissionMode) IsValid() bool {
	switch m {
	case PermissionAllow, PermissionConfirm, PermissionDeny:
		return true
	}
	return false
}

// PermissionRule defines a single capability permission.
//
// Constraints narrow what a capability can actually do. They are
// capability-specific; the permission engine doesn't interpret them, it
// just hands them to the tool at invocation time. Known keys:
//
//	command.exec:
//	  "allowed_binaries" []string — only these binary basenames may run.
//	filesystem.read / .write:
//	  "max_bytes" int (future) — cap on file size.
//	network.outgoing:
//	  "allowed_hosts" []string (future).
//
// Absent or empty Constraints means "no narrowing"; the capability applies
// with whatever the tool's own defaults are (which for command.exec still
// refuses shell metacharacters and forces a clean env).
type PermissionRule struct {
	Capability  string                 `json:"capability"`
	Mode        PermissionMode         `json:"mode"`
	Constraints map[string]interface{} `json:"constraints,omitempty"`
}

// PermissionPolicy is a collection of permission rules plus optional
// per-connection overrides (ADR 0001 §7). The override map narrows the
// default rules for a specific plugin Connection: "confirm all
// gmail.send on the work account; allow on personal." Overrides are
// additive — absent entries fall through to the Rules list exactly as
// before.
type PermissionPolicy struct {
	Rules                  []PermissionRule        `json:"rules"`
	PerConnectionOverrides []PerConnectionOverride `json:"per_connection_overrides,omitempty"`
}

// PerConnectionOverride narrows a single capability rule to a specific
// plugin Connection. Resolution order (documented in ADR 0001 §7):
// per-connection override → matching PermissionRule → implicit deny.
type PerConnectionOverride struct {
	ConnectionID string                 `json:"connection_id"`
	Capability   string                 `json:"capability"`
	Mode         PermissionMode         `json:"mode"`
	Constraints  map[string]interface{} `json:"constraints,omitempty"`
}

// Wildcard matching and policy evaluation live in the permissions package so
// there is a single canonical implementation. Use
// permissions.Engine.Evaluate(policy, capability) instead of a direct lookup
// through the domain package.

// MemoryPolicy defines how an assistant uses memory
type MemoryPolicy struct {
	Enabled         bool   `json:"enabled"`
	Scope           string `json:"scope,omitempty"`            // "profile" | "workspace"
	SummaryTemplate string `json:"summary_template,omitempty"` // optional memory summarization instruction
}

// ModelPolicy defines how an assistant selects LLM models
type ModelPolicy struct {
	Mode          string `json:"mode"`                // "global_default" | "assistant_override"
	Preferred     string `json:"preferred,omitempty"` // "provider_id:model_id"
	Fallback      string `json:"fallback,omitempty"`  // "provider_id:model_id"
	LocalOnly     bool   `json:"local_only,omitempty"`
	AllowFallback bool   `json:"allow_fallback,omitempty"` // default true
}

// ProviderProfile defines an LLM provider configuration
type ProviderProfile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"` // "local" | "remote"
	Endpoint  string    `json:"endpoint,omitempty"`
	ModelIDs  []string  `json:"model_ids"`
	SecretRef string    `json:"secret_ref,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChannelConfig defines which connections within a connector an assistant uses
type ChannelConfig struct {
	Connector   string   `json:"connector"`   // e.g., "telegram"
	Connections []string `json:"connections"` // e.g., ["bot-1", "bot-2"]
}

// RecommendedBinding suggests a plugin connection for an assistant
type RecommendedBinding struct {
	PluginID string `json:"plugin_id"`
	Role     string `json:"role"`
	Reason   string `json:"reason"`
}

// AssistantDefinition defines an assistant's configuration
type AssistantDefinition struct {
	ID                  string               `json:"id"`
	TemplateID          string               `json:"template_id,omitempty"`
	Name                string               `json:"name"`
	Tagline             string               `json:"tagline,omitempty"`
	Role                string               `json:"role"`
	BestFor             string               `json:"best_for,omitempty"`
	NotFor              string               `json:"not_for,omitempty"`
	SuggestedModel      string               `json:"suggested_model,omitempty"`
	SystemPrompt        string               `json:"system_prompt"`
	Channels            []string             `json:"channels,omitempty"`
	ChannelConfigs      []ChannelConfig      `json:"channel_configs,omitempty"`
	Capabilities        []string             `json:"capabilities,omitempty"`
	Contexts            []ContextAttachment  `json:"contexts,omitempty"`
	MemoryPolicy        MemoryPolicy         `json:"memory_policy,omitempty"`
	PermissionPolicy    PermissionPolicy     `json:"permission_policy,omitempty"`
	ModelPolicy         *ModelPolicy         `json:"model_policy,omitempty"`
	RecommendedBindings []RecommendedBinding `json:"recommended_bindings,omitempty"`
	ExecutorBackend     string               `json:"executor_backend,omitempty"`
	CreatedAt           time.Time            `json:"created_at"`
}

// MemoryEntry represents a single memory item
type MemoryEntry struct {
	ID          string    `json:"id"`
	Scope       string    `json:"scope"` // "profile" | "workspace"
	Content     string    `json:"content"`
	AssistantID *string   `json:"assistant_id,omitempty"`
	RunID       *string   `json:"run_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// EventType represents the type of an event
type EventType string

const (
	EventRunCreated        EventType = "run.created"
	EventPlanProposed      EventType = "plan.proposed"
	EventStepStarted       EventType = "step.started"
	EventStepStreaming     EventType = "step.streaming"
	EventStepCompleted     EventType = "step.completed"
	EventStepFailed        EventType = "step.failed"
	EventStepRetrying      EventType = "step.retrying"
	EventApprovalRequested EventType = "approval.requested"
	EventApprovalResolved  EventType = "approval.resolved"
	EventRunPaused         EventType = "run.paused"
	EventRunResumed        EventType = "run.resumed"
	EventRunCancelled      EventType = "run.cancelled"
	EventRunCompleted      EventType = "run.completed"
	EventRunFailed         EventType = "run.failed"
	// Conversation events (ADR 0001 §8).
	EventConversationCreated EventType = "conversation.created"
	EventConversationTouched EventType = "conversation.touched"
	EventConversationDeleted EventType = "conversation.deleted"
	// Plugin lifecycle events (lifecycle-10). Payload carries
	// plugin_id, from_version, to_version (omitted for the available
	// case when not yet installed).
	EventPluginUpdateAvailable EventType = "plugin.update_available"
	EventPluginUpdated         EventType = "plugin.updated"

	// Entity-deletion events drive memory tombstones (ADR 0004 §6). Both
	// are entity-scoped, not run-scoped — they carry the entity ID in the
	// payload and use a sentinel RunID.
	EventAssistantDeleted EventType = "assistant.deleted"
	EventRunDeleted       EventType = "run.deleted"

	// Memory audit events emitted by memstore.Client implementations on
	// every Store / Forget / Tombstone. Includes content_hash in the
	// payload so /audit/verify can verify intent without re-reading the
	// memory store.
	EventMemoryStore     EventType = "memory.store"
	EventMemoryForget    EventType = "memory.forget"
	EventMemoryTombstone EventType = "memory.tombstone"
)

// Event represents a significant occurrence in the system
type Event struct {
	ID        string                 `json:"id"`
	Type      EventType              `json:"type"`
	RunID     string                 `json:"run_id"`
	StepID    *string                `json:"step_id,omitempty"`
	Payload   map[string]interface{} `json:"payload,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}

// TriggerRule is a simple predicate over an inbound email message.
// Used by the Email plugin to route messages to specific assistants.
// Fields are optional (substring match, case-insensitive); empty fields
// are skipped. A rule with every filter empty matches everything.
type TriggerRule struct {
	Name            string `json:"name"`
	AssistantID     string `json:"assistant_id"`
	FromContains    string `json:"from_contains,omitempty"`
	SubjectContains string `json:"subject_contains,omitempty"`
	BodyContains    string `json:"body_contains,omitempty"`
	Enabled         bool   `json:"enabled"`
}
