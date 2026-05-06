package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/memory"
)

// MemoryServer handles memory-related endpoints
type MemoryServer struct {
	manager *memory.Manager
}

// NewMemoryServer creates a new memory server
func NewMemoryServer(manager *memory.Manager) *MemoryServer {
	return &MemoryServer{manager: manager}
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
		// List all by defaulting to workspace scope
		entries, err = s.manager.ListByScope("workspace", limit)
		if err == nil {
			profileEntries, _ := s.manager.ListByScope("profile", limit)
			entries = append(entries, profileEntries...)
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
