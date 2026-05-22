package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	assistanttemplates "github.com/felixgeelhaar/nomi/templates"
)

// AssistantServer handles assistant-related endpoints
type AssistantServer struct {
	repo         *db.AssistantRepository
	settingsRepo *db.AppSettingsRepository
	eventBus     *events.EventBus
}

// NewAssistantServer creates a new assistant server. eventBus may be
// nil in tests that don't exercise the deletion path; the production
// router always wires one.
func NewAssistantServer(database *db.DB, eventBus *events.EventBus) *AssistantServer {
	return &AssistantServer{
		repo:         db.NewAssistantRepository(database),
		settingsRepo: db.NewAppSettingsRepository(database),
		eventBus:     eventBus,
	}
}

// respondCeilingValidationError translates a CeilingValidationError into a
// 400 with a structured body the UI can render: human message + the list of
// inert rules + a suggested capabilities array the user can apply with one
// click. Falls back to a plain error envelope if the error is some other
// shape.
func respondCeilingValidationError(c *gin.Context, err error) {
	var cve *permissions.CeilingValidationError
	if e, ok := err.(*permissions.CeilingValidationError); ok {
		cve = e
	}
	if cve == nil {
		respondValidationError(c, err.Error())
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{
		"error":                  cve.Error(),
		"code":                   "ceiling_violation",
		"violations":             cve.Violations,
		"suggested_capabilities": cve.SuggestedCapabilities,
	})
}

// CreateAssistantRequest represents a request to create an assistant
type CreateAssistantRequest struct {
	Name             string                     `json:"name" binding:"required"`
	TemplateID       string                     `json:"template_id,omitempty"`
	Tagline          string                     `json:"tagline,omitempty"`
	Role             string                     `json:"role" binding:"required"`
	BestFor          string                     `json:"best_for,omitempty"`
	NotFor           string                     `json:"not_for,omitempty"`
	SuggestedModel   string                     `json:"suggested_model,omitempty"`
	SystemPrompt     string                     `json:"system_prompt" binding:"required"`
	Channels         []string                   `json:"channels,omitempty"`
	ChannelConfigs   []domain.ChannelConfig     `json:"channel_configs,omitempty"`
	Capabilities     []string                   `json:"capabilities,omitempty"`
	Contexts         []domain.ContextAttachment `json:"contexts,omitempty"`
	MemoryPolicy     domain.MemoryPolicy        `json:"memory_policy,omitempty"`
	PermissionPolicy domain.PermissionPolicy    `json:"permission_policy,omitempty"`
	ModelPolicy      *domain.ModelPolicy        `json:"model_policy,omitempty"`
	ExecutorBackend  string                     `json:"executor_backend,omitempty"`
	SandboxImage     string                     `json:"sandbox_image,omitempty"`
}

// ListTemplates returns bundled assistant templates.
func (s *AssistantServer) ListTemplates(c *gin.Context) {
	tpls, err := assistanttemplates.BuiltIn()
	if err != nil {
		respondInternal(c, "failed to load templates", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"templates": tpls})
}

// CreateAssistant creates a new assistant
func (s *AssistantServer) CreateAssistant(c *gin.Context) {
	var req CreateAssistantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	// Use default policy if none provided
	permPolicy := req.PermissionPolicy
	if len(permPolicy.Rules) == 0 {
		safetyProfile := s.settingsRepo.GetOrDefault("safety_profile", permissions.DefaultSafetyProfile)
		permPolicy = permissions.BuildSafetyProfilePolicy(safetyProfile)
	}

	// Reject policies whose non-deny rules reference a capability whose
	// family is not declared — those rules are silently ineffective at
	// runtime and produce a confusing "permission denied" with no path
	// to the fix. Loud failure here lets the UI render an actionable
	// error with a one-click "add families" remedy.
	if err := permissions.ValidatePolicyAgainstCeiling(req.Capabilities, permPolicy); err != nil {
		respondCeilingValidationError(c, err)
		return
	}

	assistant := &domain.AssistantDefinition{
		ID:               uuid.New().String(),
		TemplateID:       req.TemplateID,
		Name:             req.Name,
		Tagline:          req.Tagline,
		Role:             req.Role,
		BestFor:          req.BestFor,
		NotFor:           req.NotFor,
		SuggestedModel:   req.SuggestedModel,
		SystemPrompt:     req.SystemPrompt,
		Channels:         req.Channels,
		ChannelConfigs:   req.ChannelConfigs,
		Capabilities:     req.Capabilities,
		Contexts:         req.Contexts,
		MemoryPolicy:     req.MemoryPolicy,
		PermissionPolicy: permPolicy,
		ModelPolicy:      req.ModelPolicy,
		ExecutorBackend:  req.ExecutorBackend,
		SandboxImage:     req.SandboxImage,
		CreatedAt:        time.Now().UTC(),
	}

	if err := s.repo.Create(assistant); err != nil {
		respondInternal(c, "failed to create assistant", err)
		return
	}

	c.JSON(http.StatusCreated, assistant)
}

// GetAssistant retrieves an assistant by ID
func (s *AssistantServer) GetAssistant(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	assistant, err := s.repo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, assistant)
}

// ListAssistants lists all assistants
func (s *AssistantServer) ListAssistants(c *gin.Context) {
	assistants, err := s.repo.List(100, 0)
	if err != nil {
		respondInternal(c, "failed to list assistants", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"assistants": assistants})
}

// UpdateAssistant updates an assistant
func (s *AssistantServer) UpdateAssistant(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	var req CreateAssistantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	assistant, err := s.repo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	if err := permissions.ValidatePolicyAgainstCeiling(req.Capabilities, req.PermissionPolicy); err != nil {
		respondCeilingValidationError(c, err)
		return
	}

	assistant.Name = req.Name
	assistant.TemplateID = req.TemplateID
	assistant.Tagline = req.Tagline
	assistant.Role = req.Role
	assistant.BestFor = req.BestFor
	assistant.NotFor = req.NotFor
	assistant.SuggestedModel = req.SuggestedModel
	assistant.SystemPrompt = req.SystemPrompt
	assistant.Channels = req.Channels
	assistant.ChannelConfigs = req.ChannelConfigs
	assistant.Capabilities = req.Capabilities
	assistant.Contexts = req.Contexts
	assistant.MemoryPolicy = req.MemoryPolicy
	assistant.PermissionPolicy = req.PermissionPolicy
	assistant.ModelPolicy = req.ModelPolicy
	assistant.ExecutorBackend = req.ExecutorBackend
	assistant.SandboxImage = req.SandboxImage

	if err := s.repo.Update(assistant); err != nil {
		respondInternal(c, "failed to update assistant", err)
		return
	}

	c.JSON(http.StatusOK, assistant)
}

// DeleteAssistant deletes an assistant
func (s *AssistantServer) DeleteAssistant(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.repo.Delete(id); err != nil {
		respondInternal(c, "failed to delete assistant", err)
		return
	}

	// Publish entity-scoped event so the runtime can tombstone any memory
	// rows still referencing this assistant (ADR 0004 §6). Best-effort —
	// failure to publish does not surface as a delete failure, but it does
	// leave orphaned memory until the next sweep.
	if s.eventBus != nil {
		_, _ = s.eventBus.Publish(c.Request.Context(), domain.EventAssistantDeleted, "", nil, map[string]interface{}{
			"assistant_id": id,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// ApplySafetyProfileToAssistant applies the current global safety profile
// policy to an existing assistant.
func (s *AssistantServer) ApplySafetyProfileToAssistant(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	assistant, err := s.repo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	profile := s.settingsRepo.GetOrDefault("safety_profile", permissions.DefaultSafetyProfile)
	assistant.PermissionPolicy = permissions.BuildSafetyProfilePolicy(profile)

	if err := s.repo.Update(assistant); err != nil {
		respondInternal(c, "failed to apply safety profile", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "applied", "profile": profile, "assistant": assistant})
}
