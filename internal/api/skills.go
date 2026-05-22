package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/felixgeelhaar/nomi/internal/recipes"
	"github.com/felixgeelhaar/nomi/internal/skills"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// SkillsServer handles the /skills surface — induction suggestions and
// promotion of a suggestion into a Recipe.
type SkillsServer struct {
	runs       *db.RunRepository
	recipes    *db.RecipeRepository
	assistants *db.AssistantRepository
}

// NewSkillsServer wires the handlers.
func NewSkillsServer(runs *db.RunRepository, recipes *db.RecipeRepository, assistants *db.AssistantRepository) *SkillsServer {
	return &SkillsServer{runs: runs, recipes: recipes, assistants: assistants}
}

// ListSuggestions runs an induction pass on demand and returns the
// current suggestions. Cheap enough to call per page-load; the pass
// scans up to MaxSourceRuns successful runs and clusters them.
func (s *SkillsServer) ListSuggestions(c *gin.Context) {
	out, err := skills.Induce(s.runs, skills.DefaultConfig())
	if err != nil {
		respondInternal(c, "induction failed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"suggestions": out})
}

// promoteRequest converts a Suggestion into a Recipe + Assistant. The
// caller may pass a source_assistant_id whose capability set is copied
// into the new recipe; without it the recipe ships with a minimal
// read-only policy and the user can tighten/widen post-install.
type promoteRequest struct {
	SuggestionID     string `json:"suggestion_id" binding:"required"`
	RecipeID         string `json:"recipe_id" binding:"required"`
	Name             string `json:"name" binding:"required"`
	Description      string `json:"description,omitempty"`
	SourceAssistant  string `json:"source_assistant_id,omitempty"`
}

// PromoteSuggestion materialises a Recipe + a new Assistant from a
// Suggestion. Re-runs the induction pass to find the suggestion (a
// stable hash over source run IDs) before promoting — guards against
// stale suggestion IDs the UI may have buffered after a corpus shift.
func (s *SkillsServer) PromoteSuggestion(c *gin.Context) {
	var req promoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	suggestions, err := skills.Induce(s.runs, skills.DefaultConfig())
	if err != nil {
		respondInternal(c, "induction failed", err)
		return
	}
	var target *skills.Suggestion
	for i := range suggestions {
		if suggestions[i].ID == req.SuggestionID {
			target = &suggestions[i]
			break
		}
	}
	if target == nil {
		respondNotFound(c, "suggestion not found — corpus may have shifted; refresh")
		return
	}

	// Build the assistant spec for the recipe. Capabilities default to
	// filesystem.read only; if the user supplied a source assistant, copy
	// its capabilities + permission policy as the starting point.
	spec := recipes.AssistantSpec{
		Name:         req.Name,
		Role:         "induced skill",
		SystemPrompt: target.RepresentativeGoal,
		Capabilities: []string{"filesystem.read"},
	}
	if req.SourceAssistant != "" && s.assistants != nil {
		if src, err := s.assistants.GetByID(req.SourceAssistant); err == nil && src != nil {
			spec.Role = src.Role
			spec.Capabilities = src.Capabilities
			spec.PermissionPolicy = src.PermissionPolicy
			spec.MemoryPolicy = src.MemoryPolicy
			spec.ExecutorBackend = src.ExecutorBackend
			spec.SandboxImage = src.SandboxImage
		}
	}

	description := req.Description
	if description == "" {
		description = "Induced from " + commaJoin(target.SourceRunIDs)
	}

	r := &recipes.Recipe{
		SchemaVersion: recipes.SchemaVersion,
		ID:            req.RecipeID,
		Name:          req.Name,
		Version:       "0.1.0",
		Author:        "induction",
		Description:   description,
		Tags:          target.CommonTokens,
		Assistant:     spec,
	}
	yaml, err := recipes.Marshal(r)
	if err != nil {
		respondValidationError(c, "recipe is invalid: "+err.Error())
		return
	}
	hash, _ := r.Hash()

	row := &db.RecipeRow{
		ID:          r.ID,
		Name:        r.Name,
		Version:     r.Version,
		Author:      r.Author,
		Description: r.Description,
		Tags:        r.Tags,
		YAML:        string(yaml),
		SHA256:      hash,
		Source:      "induced",
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.recipes.Upsert(row); err != nil {
		respondInternal(c, "failed to persist recipe", err)
		return
	}

	assistant := r.ToAssistantDefinition()
	assistant.ID = uuid.New().String()
	assistant.TemplateID = "recipe:" + r.ID
	assistant.CreatedAt = time.Now().UTC()
	if err := s.assistants.Create(assistant); err != nil {
		respondInternal(c, "failed to create assistant", err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"recipe":         r,
		"sha256":         hash,
		"assistant":      assistant,
		"source_run_ids": target.SourceRunIDs,
	})
}

func commaJoin(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, ", ")
}
