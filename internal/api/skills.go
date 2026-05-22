package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/felixgeelhaar/nomi/internal/llm"
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
	llm        *llm.Resolver
}

// NewSkillsServer wires the handlers. llmResolver is optional — when
// nil the /skills/synthesize endpoint returns 503 but the cheaper
// heuristic-only suggestions + promote paths still work.
func NewSkillsServer(runs *db.RunRepository, recipes *db.RecipeRepository, assistants *db.AssistantRepository, llmResolver *llm.Resolver) *SkillsServer {
	return &SkillsServer{runs: runs, recipes: recipes, assistants: assistants, llm: llmResolver}
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
//
// The synthesized_* fields, when set, override the defaults derived
// from source_assistant_id. They carry the LLM synthesis output so a
// "Generate with AI" pre-fill survives the round-trip into Recipe
// land without the UI having to call multiple endpoints.
type promoteRequest struct {
	SuggestionID         string   `json:"suggestion_id" binding:"required"`
	RecipeID             string   `json:"recipe_id" binding:"required"`
	Name                 string   `json:"name" binding:"required"`
	Description          string   `json:"description,omitempty"`
	SourceAssistant      string   `json:"source_assistant_id,omitempty"`
	SynthesizedRole      string   `json:"synthesized_role,omitempty"`
	SynthesizedPrompt    string   `json:"synthesized_system_prompt,omitempty"`
	SynthesizedCapsList  []string `json:"synthesized_capabilities,omitempty"`
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
	// LLM-synthesized fields win over source-assistant defaults — they
	// came from a deliberate "Generate with AI" the user opted into.
	if req.SynthesizedRole != "" {
		spec.Role = req.SynthesizedRole
	}
	if req.SynthesizedPrompt != "" {
		spec.SystemPrompt = req.SynthesizedPrompt
	}
	if len(req.SynthesizedCapsList) > 0 {
		spec.Capabilities = req.SynthesizedCapsList
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

type synthesizeRequest struct {
	SuggestionID string `json:"suggestion_id" binding:"required"`
}

// Synthesize (POST /skills/synthesize) calls the LLM to produce a
// proposed Recipe shape from a Suggestion. Re-runs induction to locate
// the cluster, fetches the source runs' goals from the state store,
// and asks the LLM to generalise a reusable system_prompt + capability
// set. Returns the proposal; the UI pre-fills the promote form with
// it before the user confirms.
func (s *SkillsServer) Synthesize(c *gin.Context) {
	if s.llm == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no LLM provider configured; configure one in Settings → AI Providers to enable skill synthesis"})
		return
	}
	var req synthesizeRequest
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

	goals, err := s.fetchClusterGoals(target.SourceRunIDs)
	if err != nil {
		respondInternal(c, "failed to load cluster goals", err)
		return
	}
	client, _, err := s.llm.DefaultClient()
	if err != nil || client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "default LLM provider is unavailable"})
		return
	}
	out, err := skills.Synthesize(c.Request.Context(), client, *target, goals)
	if err != nil {
		respondInternal(c, "synthesis failed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"suggestion": target,
		"recipe":     out,
	})
}

// fetchClusterGoals loads each source run's goal text. Runs that have
// been deleted since the suggestion was generated are skipped silently
// — the cluster centroid still has meaning even if a few rows are
// missing.
func (s *SkillsServer) fetchClusterGoals(runIDs []string) ([]string, error) {
	out := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		run, err := s.runs.GetByID(id)
		if err != nil || run == nil {
			continue
		}
		out = append(out, run.Goal)
	}
	return out, nil
}

func commaJoin(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, ", ")
}
