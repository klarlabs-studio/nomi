package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/runtime"
)

// Server holds all API handlers
type Server struct {
	runtime *runtime.Runtime
}

// NewServer creates a new API server
func NewServer(rt *runtime.Runtime) *Server {
	return &Server{runtime: rt}
}

// CreateRunRequest represents a request to create a run
type CreateRunRequest struct {
	Goal        string `json:"goal" binding:"required"`
	AssistantID string `json:"assistant_id" binding:"required"`
}

// CreateRun creates a new run
func (s *Server) CreateRun(c *gin.Context) {
	var req CreateRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	run, err := s.runtime.CreateRun(c.Request.Context(), req.Goal, req.AssistantID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, err)
		return
	}

	c.JSON(http.StatusCreated, run)
}

// GetRun retrieves a run by ID
func (s *Server) GetRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	run, steps, plan, err := s.runtime.GetRun(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"run":   run,
		"steps": steps,
		"plan":  plan,
	})
}

// ListRuns lists all runs. When ?search=<q> is set, the result is
// filtered to runs whose goal or any owned step title matches the
// query (case-insensitive substring). Powers the chat-list search box.
func (s *Server) ListRuns(c *gin.Context) {
	if q := c.Query("search"); q != "" {
		runs, err := s.runtime.SearchRuns(q, 50)
		if err != nil {
			respondInternal(c, "failed to search runs", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"runs": runs})
		return
	}
	runs, err := s.runtime.ListRuns()
	if err != nil {
		respondInternal(c, "failed to list runs", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

// ApproveRun approves a pending run
func (s *Server) ApproveRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.ApproveRun(c.Request.Context(), id); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "approved"})
}

// GetRunApprovals gets all approvals for a run
func (s *Server) GetRunApprovals(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	approvals, err := s.runtime.GetRunApprovals(id)
	if err != nil {
		respondInternal(c, "failed to list run approvals", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"approvals": approvals})
}

// RetryRun retries a failed run
func (s *Server) RetryRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.RetryRun(c.Request.Context(), id); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "retrying"})
}

// PauseRun pauses an active run
func (s *Server) PauseRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.PauseRun(c.Request.Context(), id); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "paused"})
}

// ResumeRun resumes a paused run
func (s *Server) ResumeRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.ResumeRun(c.Request.Context(), id); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "resumed"})
}

// CancelRun cancels an active run
func (s *Server) CancelRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.CancelRun(c.Request.Context(), id); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "cancelled"})
}

// ReplanRun triggers a manual replan against the run's last failed
// step. The user-facing CTA "Fix this with the agent" lives next to
// the failed-step banner in chat-detail.tsx; the desktop app POSTs
// here. The backend re-uses Runtime.Replan, which is also called
// automatically from the executor. Bounded by MaxReplansPerRun.
func (s *Server) ReplanRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	run, _, _, err := s.runtime.GetRun(id)
	if err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}
	if !run.Status.IsTerminal() {
		respondValidationError(c, "manual replan is only allowed on a terminal (failed) run")
		return
	}

	steps, err := s.runtime.ManualReplan(c.Request.Context(), id)
	if err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "replanned", "step_count": len(steps)})
}

// ApprovePlan approves the proposed plan for a run
func (s *Server) ApprovePlan(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.ApprovePlan(c.Request.Context(), id); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "plan approved"})
}

// EditPlanRequest represents a request to edit a plan
type EditPlanRequest struct {
	Steps []struct {
		ID                 string   `json:"id,omitempty"`
		Title              string   `json:"title"`
		Description        string   `json:"description,omitempty"`
		ExpectedTool       string   `json:"expected_tool,omitempty"`
		ExpectedCapability string   `json:"expected_capability,omitempty"`
		DependsOn          []string `json:"depends_on,omitempty"`
	} `json:"steps" binding:"required"`
}

// EditPlan updates the plan for a run
func (s *Server) EditPlan(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	var req EditPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	stepDefs := make([]domain.StepDefinition, len(req.Steps))
	for i, s := range req.Steps {
		stepDefs[i] = domain.StepDefinition{
			ID:                 s.ID,
			Title:              s.Title,
			Description:        s.Description,
			ExpectedTool:       s.ExpectedTool,
			ExpectedCapability: s.ExpectedCapability,
			DependsOn:          s.DependsOn,
		}
	}

	if err := s.runtime.EditPlan(c.Request.Context(), id, stepDefs); err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "plan updated"})
}

// ForkRunRequest represents a request to branch a run from a specific step

type ForkRunRequest struct {
	StepID string `json:"step_id" binding:"required"`
	Goal   string `json:"goal,omitempty"`
}

// ForkRun creates a new run branched from an existing run at a specific step.
// The child run inherits the parent's assistant and context up to the branch point.
func (s *Server) ForkRun(c *gin.Context) {
	parentID := c.Param("id")
	if parentID == "" {
		respondValidationError(c, "id is required")
		return
	}

	var req ForkRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	childRun, err := s.runtime.ForkRun(c.Request.Context(), parentID, req.StepID, req.Goal)
	if err != nil {
		respondError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"run": childRun})
}

// DeleteRun deletes a run
func (s *Server) DeleteRun(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	if err := s.runtime.DeleteRun(id); err != nil {
		respondInternal(c, "failed to delete run", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
