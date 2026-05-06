package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// ToolsServer handles tool-related endpoints
type ToolsServer struct {
	executor *tools.Executor
}

// NewToolsServer creates a new tools server
func NewToolsServer(registry *tools.Registry) *ToolsServer {
	return &ToolsServer{
		executor: tools.NewExecutor(registry),
	}
}

// PreviewFolderContextRequest represents a request to preview folder context
//
//	type PreviewFolderContextRequest struct {
//		Path     string `json:"path" binding:"required"`
//		MaxDepth int    `json:"max_depth"`
//	}
type PreviewFolderContextRequest struct {
	Path     string `json:"path" binding:"required"`
	MaxDepth int    `json:"max_depth"`
}

// PreviewFolderContext previews a folder's structure without requiring a run
func (s *ToolsServer) PreviewFolderContext(c *gin.Context) {
	var req PreviewFolderContextRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	if req.MaxDepth <= 0 {
		req.MaxDepth = 3
	}

	result := s.executor.Execute(c.Request.Context(), "filesystem.context", map[string]interface{}{
		"path":      req.Path,
		"max_depth": req.MaxDepth,
	})

	if !result.Success {
		respondInternal(c, "failed to preview folder context", fmt.Errorf("%s", result.Error))
		return
	}

	c.JSON(http.StatusOK, result.Output)
}
