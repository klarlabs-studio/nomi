package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"go.klarlabs.de/nomi/internal/recipes"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// RecipeServer handles the /recipes REST surface.
type RecipeServer struct {
	repo          *db.RecipeRepository
	assistantRepo *db.AssistantRepository
}

// NewRecipeServer wires the recipe handlers.
func NewRecipeServer(repo *db.RecipeRepository, assistants *db.AssistantRepository) *RecipeServer {
	return &RecipeServer{repo: repo, assistantRepo: assistants}
}

// catalogEntry is the JSON shape returned by GET /recipes. Combines
// metadata from a parsed recipe with a flag indicating whether the
// entry comes from the built-in catalog or the local recipes table.
type catalogEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Author      string   `json:"author,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Source      string   `json:"source"` // "builtin" | "imported" | "exported"
	SHA256      string   `json:"sha256,omitempty"`
}

// ListRecipes returns the union of the built-in catalog and any rows in
// the recipes table. Built-in entries are surfaced even when no rows
// have been imported yet — the assistant builder always has something
// to render.
func (s *RecipeServer) ListRecipes(c *gin.Context) {
	out := []catalogEntry{}

	builtin, err := recipes.BuiltInCatalog()
	if err == nil {
		for _, r := range builtin {
			hash, _ := r.Hash()
			out = append(out, catalogEntry{
				ID: r.ID, Name: r.Name, Version: r.Version,
				Author: r.Author, Description: r.Description, Tags: r.Tags,
				Source: "builtin", SHA256: hash,
			})
		}
	}

	rows, err := s.repo.List()
	if err == nil {
		for _, row := range rows {
			out = append(out, catalogEntry{
				ID: row.ID, Name: row.Name, Version: row.Version,
				Author: row.Author, Description: row.Description, Tags: row.Tags,
				Source: row.Source, SHA256: row.SHA256,
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"recipes": out})
}

// GetRecipe returns the full recipe (parsed YAML + hash) for a single
// ID. Built-in entries are checked first; falls back to the repository.
func (s *RecipeServer) GetRecipe(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	if r, err := recipes.BuiltInByID(id); err == nil {
		hash, _ := r.Hash()
		c.JSON(http.StatusOK, gin.H{
			"recipe": r,
			"sha256": hash,
			"source": "builtin",
		})
		return
	}
	row, err := s.repo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	r, err := recipes.Parse([]byte(row.YAML))
	if err != nil {
		respondInternal(c, "stored recipe is malformed", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"recipe": r,
		"sha256": row.SHA256,
		"source": row.Source,
	})
}

// installRequest controls the install flow. `expected_sha256`, if set,
// is checked against the recipe's actual hash so the caller can pin a
// specific version even on a built-in catalog that may evolve.
type installRequest struct {
	ID             string `json:"id" binding:"required"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

// installPreview is the JSON returned by /recipes/:id/preview — same
// data the install flow would write, so the UI can render a diff /
// confirmation step before calling /install.
type installPreview struct {
	Recipe           *recipes.Recipe        `json:"recipe"`
	SHA256           string                 `json:"sha256"`
	AssistantPreview map[string]interface{} `json:"assistant_preview"`
}

// PreviewInstall returns what InstallRecipe would write without making
// any changes. Used by the UI's confirmation step.
func (s *RecipeServer) PreviewInstall(c *gin.Context) {
	r, hash, err := s.resolveRecipe(c.Param("id"))
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	a := r.ToAssistantDefinition()
	c.JSON(http.StatusOK, installPreview{
		Recipe: r,
		SHA256: hash,
		AssistantPreview: map[string]interface{}{
			"name":              a.Name,
			"role":              a.Role,
			"capabilities":      a.Capabilities,
			"permission_policy": a.PermissionPolicy,
			"executor_backend":  a.ExecutorBackend,
		},
	})
}

// InstallRecipe creates a new assistant from a recipe. ExpectedSHA256,
// when supplied, must match the recipe's actual hash before the install
// proceeds — guards against a built-in catalog shifting under the
// caller's feet.
func (s *RecipeServer) InstallRecipe(c *gin.Context) {
	var req installRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	r, hash, err := s.resolveRecipe(req.ID)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	if req.ExpectedSHA256 != "" && req.ExpectedSHA256 != hash {
		respondValidationError(c, "recipe sha256 mismatch — catalog may have updated since the preview")
		return
	}
	assistant := r.ToAssistantDefinition()
	assistant.ID = uuid.New().String()
	assistant.TemplateID = "recipe:" + r.ID
	assistant.CreatedAt = time.Now().UTC()
	if err := s.assistantRepo.Create(assistant); err != nil {
		respondInternal(c, "failed to create assistant", err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"assistant": assistant,
		"recipe_id": r.ID,
		"sha256":    hash,
	})
}

// ExportRecipe materialises a recipe from an existing assistant, hashes
// it, and persists the YAML to the recipes table tagged as 'exported'.
// Returns the recipe document so the caller can download or share it.
func (s *RecipeServer) ExportRecipe(c *gin.Context) {
	assistantID := c.Query("assistant_id")
	if assistantID == "" {
		respondValidationError(c, "assistant_id is required")
		return
	}
	a, err := s.assistantRepo.GetByID(assistantID)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	var body struct {
		ID      string `json:"id,omitempty"`
		Version string `json:"version,omitempty"`
	}
	_ = c.ShouldBindJSON(&body)
	r, err := recipes.FromAssistant(body.ID, body.Version, a)
	if err != nil {
		respondInternal(c, "failed to build recipe", err)
		return
	}
	yaml, err := recipes.Marshal(r)
	if err != nil {
		respondInternal(c, "failed to marshal recipe", err)
		return
	}
	hash, err := r.Hash()
	if err != nil {
		respondInternal(c, "failed to hash recipe", err)
		return
	}
	row := &db.RecipeRow{
		ID:          r.ID,
		Name:        r.Name,
		Version:     r.Version,
		Author:      r.Author,
		Description: r.Description,
		Tags:        r.Tags,
		YAML:        string(yaml),
		SHA256:      hash,
		Source:      "exported",
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.repo.Upsert(row); err != nil {
		respondInternal(c, "failed to persist exported recipe", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"recipe": r,
		"sha256": hash,
		"yaml":   string(yaml),
	})
}

// resolveRecipe finds a recipe by ID across the built-in catalog and
// the local repository. Returns the parsed recipe + its sha256.
func (s *RecipeServer) resolveRecipe(id string) (*recipes.Recipe, string, error) {
	if id == "" {
		return nil, "", recipeNotFoundErr(id)
	}
	if r, err := recipes.BuiltInByID(id); err == nil {
		hash, _ := r.Hash()
		return r, hash, nil
	}
	row, err := s.repo.GetByID(id)
	if err != nil {
		return nil, "", recipeNotFoundErr(id)
	}
	r, err := recipes.Parse([]byte(row.YAML))
	if err != nil {
		return nil, "", err
	}
	return r, row.SHA256, nil
}

type recipeError struct{ id string }

func (e recipeError) Error() string { return "recipe not found: " + e.id }

func recipeNotFoundErr(id string) error { return recipeError{id: id} }
