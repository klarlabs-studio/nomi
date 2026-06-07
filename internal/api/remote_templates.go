package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// RemoteTemplateServer handles remote assistant template marketplace.
type RemoteTemplateServer struct {
	repo *db.RemoteTemplateRepository
}

// NewRemoteTemplateServer creates a new server.
func NewRemoteTemplateServer(repo *db.RemoteTemplateRepository) *RemoteTemplateServer {
	return &RemoteTemplateServer{repo: repo}
}

// ListRemoteTemplates returns all installed remote templates.
func (s *RemoteTemplateServer) ListRemoteTemplates(c *gin.Context) {
	templates, err := s.repo.ListAll()
	if err != nil {
		respondInternal(c, "failed to list remote templates", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"templates": templates})
}

// InstallRemoteTemplate installs a remote template as a local draft assistant.
func (s *RemoteTemplateServer) InstallRemoteTemplate(c *gin.Context) {
	var req struct {
		ID                  string `json:"id"`
		CatalogHash         string `json:"catalog_hash"`
		SourceURL           string `json:"source_url"`
		Signature           string `json:"signature"`
		Name                string `json:"name"`
		Tagline             string `json:"tagline"`
		Role                string `json:"role"`
		BestFor             string `json:"best_for"`
		NotFor              string `json:"not_for"`
		SuggestedModel      string `json:"suggested_model"`
		SystemPrompt        string `json:"system_prompt"`
		Channels            string `json:"channels"`             // JSON array
		Capabilities        string `json:"capabilities"`         // JSON array
		Contexts            string `json:"contexts"`             // JSON array
		MemoryPolicy        string `json:"memory_policy"`        // JSON object
		PermissionPolicy    string `json:"permission_policy"`    // JSON object
		RecommendedBindings string `json:"recommended_bindings"` // JSON array
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, "invalid request: "+err.Error())
		return
	}
	if req.ID == "" || req.SourceURL == "" {
		respondValidationError(c, "id and source_url are required")
		return
	}

	// Generate a local assistant ID (draft, not active)
	assistantID := "asst-" + req.ID

	rt := &db.RemoteTemplate{
		ID:                  req.ID,
		CatalogHash:         req.CatalogHash,
		SourceURL:           req.SourceURL,
		Signature:           req.Signature,
		Name:                req.Name,
		Tagline:             req.Tagline,
		Role:                req.Role,
		BestFor:             req.BestFor,
		NotFor:              req.NotFor,
		SuggestedModel:      req.SuggestedModel,
		SystemPrompt:        req.SystemPrompt,
		Channels:            req.Channels,
		Capabilities:        req.Capabilities,
		Contexts:            req.Contexts,
		MemoryPolicy:        req.MemoryPolicy,
		PermissionPolicy:    req.PermissionPolicy,
		RecommendedBindings: req.RecommendedBindings,
		LocalAssistantID:    assistantID,
	}

	if err := s.repo.Install(rt, assistantID); err != nil {
		respondInternal(c, "failed to install template", err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"assistant_id": assistantID, "status": "draft"})
}
