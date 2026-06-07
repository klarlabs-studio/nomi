package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/memory"
	"go.klarlabs.de/nomi/internal/memstore"
)

// MemoryServer handles memory-related endpoints
type MemoryServer struct {
	manager *memory.Manager
	client  memstore.Client // optional; powers /export and /import (ADR 0004 §8)
}

// NewMemoryServer creates a new memory server. client may be nil; the
// /export and /import endpoints return 503 when it is.
func NewMemoryServer(manager *memory.Manager, client memstore.Client) *MemoryServer {
	return &MemoryServer{manager: manager, client: client}
}

// CreateMemoryRequest represents a request to create a memory entry
type CreateMemoryRequest struct {
	Content     string  `json:"content" binding:"required"`
	Scope       string  `json:"scope"`
	AssistantID *string `json:"assistant_id,omitempty"`
	RunID       *string `json:"run_id,omitempty"`
}

// CreateMemory creates a new memory entry
func (s *MemoryServer) CreateMemory(c *gin.Context) {
	var req CreateMemoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	entry := &domain.MemoryEntry{
		Scope:       req.Scope,
		Content:     req.Content,
		AssistantID: req.AssistantID,
		RunID:       req.RunID,
	}

	if entry.Scope == "" {
		entry.Scope = "workspace"
	}

	if err := s.manager.Save(entry); err != nil {
		respondInternal(c, "failed to create memory", err)
		return
	}

	c.JSON(http.StatusCreated, entry)
}

// ListMemory lists memory entries with optional filtering
func (s *MemoryServer) ListMemory(c *gin.Context) {
	scope := c.Query("scope")
	query := c.Query("q")
	limitStr := c.DefaultQuery("limit", "100")

	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		respondValidationError(c, "invalid limit")
		return
	}

	var entries []*domain.MemoryEntry

	if query != "" {
		entries, err = s.manager.Search(scope, query, limit)
	} else if scope != "" {
		entries, err = s.manager.ListByScope(scope, limit)
	} else {
		// No scope filter — return the union of every user-visible
		// scope so the UI's Memory tab can render workspace, profile,
		// and preferences (learned + manual) without making three
		// separate calls.
		entries, err = s.manager.ListByScope("workspace", limit)
		if err == nil {
			profileEntries, _ := s.manager.ListByScope("profile", limit)
			entries = append(entries, profileEntries...)
			preferenceEntries, _ := s.manager.ListByScope("preferences", limit)
			entries = append(entries, preferenceEntries...)
		}
	}

	if err != nil {
		respondInternal(c, "failed to list memories", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"memories": entries})
}

// GetMemory retrieves a memory entry by ID
func (s *MemoryServer) GetMemory(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	entry, err := s.manager.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, entry)
}

// DeleteMemory deletes a memory entry
func (s *MemoryServer) DeleteMemory(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.manager.Delete(id); err != nil {
		respondInternal(c, "failed to delete memory", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// ExportMemory streams a Mnemos JSONL export of the requested scope.
// Query params:
//
//	scope — workspace | profile | preferences (default: workspace)
//	key   — workspace key (default: "default" for workspace; empty for
//	        profile/preferences which take no key)
//
// Content-Type is application/x-ndjson; the first line is the export
// header, subsequent lines are entries.
func (s *MemoryServer) ExportMemory(c *gin.Context) {
	if s.client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory export unavailable: no memstore.Client wired"})
		return
	}
	scope, err := parseScopeQuery(c)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="mnemos-%s.jsonl"`, scope.Kind))
	if _, err := memory.Export(c.Request.Context(), s.client, scope, c.Writer); err != nil {
		// Body may already be partly written; can't switch status code.
		// Trailer-style error is unfortunate but matches streaming endpoints.
		_, _ = fmt.Fprintf(c.Writer, "\n# export error: %s\n", err.Error())
		return
	}
}

// ImportMemory reads a Mnemos JSONL stream from the request body and
// stores every entry through the configured client. Returns the count
// imported. Body must start with a valid header line; mismatched
// format/version is a 400.
func (s *MemoryServer) ImportMemory(c *gin.Context) {
	if s.client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory import unavailable: no memstore.Client wired"})
		return
	}
	n, err := memory.Import(c.Request.Context(), s.client, c.Request.Body)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"imported": n})
}

// parseScopeQuery builds a memstore.Scope from the request's scope/key
// query params, applying the embedded-mode defaults. Returns an error
// rather than panicking so the handler can surface a 400.
func parseScopeQuery(c *gin.Context) (memstore.Scope, error) {
	kindStr := c.DefaultQuery("scope", string(memstore.ScopeWorkspace))
	kind := memstore.ScopeKind(kindStr)
	if !kind.IsValid() {
		return memstore.Scope{}, fmt.Errorf("invalid scope %q", kindStr)
	}
	key := c.Query("key")
	scope := memstore.Scope{OwnerID: memstore.LocalOwnerID, Kind: kind, Key: key}
	// Fill in canonical defaults for kinds that require a key when none
	// was supplied — keeps the CLI ergonomics simple for the common case.
	switch kind {
	case memstore.ScopeWorkspace, memstore.ScopeSession, memstore.ScopeOrg:
		if scope.Key == "" {
			scope.Key = memstore.DefaultWorkspaceKey
		}
	case memstore.ScopeProfile, memstore.ScopePreferences:
		scope.Key = ""
	}
	if err := memstore.ValidateScope(scope); err != nil {
		return memstore.Scope{}, err
	}
	return scope, nil
}
