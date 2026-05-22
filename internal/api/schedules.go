package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/felixgeelhaar/nomi/internal/scheduler"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// ScheduleServer handles REST CRUD for schedules.
type ScheduleServer struct {
	repo *db.ScheduleRepository
	sch  *scheduler.Scheduler
}

// NewScheduleServer wires the schedule REST handlers. The scheduler is
// consulted for cron validation + next-fire calculation; the repository
// owns persistence.
func NewScheduleServer(repo *db.ScheduleRepository, sch *scheduler.Scheduler) *ScheduleServer {
	return &ScheduleServer{repo: repo, sch: sch}
}

type createScheduleRequest struct {
	AssistantID string `json:"assistant_id" binding:"required"`
	Prompt      string `json:"prompt" binding:"required"`
	CronExpr    string `json:"cron_expr" binding:"required"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

// CreateSchedule (POST /schedules).
func (s *ScheduleServer) CreateSchedule(c *gin.Context) {
	var req createScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	sched, err := s.sch.NewSchedule(req.AssistantID, req.Prompt, req.CronExpr)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	if req.Enabled != nil {
		sched.Enabled = *req.Enabled
	}
	if err := s.repo.Create(sched); err != nil {
		respondInternal(c, "failed to create schedule", err)
		return
	}
	c.JSON(http.StatusCreated, sched)
}

// ListSchedules (GET /schedules).
func (s *ScheduleServer) ListSchedules(c *gin.Context) {
	out, err := s.repo.List()
	if err != nil {
		respondInternal(c, "failed to list schedules", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedules": out})
}

// GetSchedule (GET /schedules/:id).
func (s *ScheduleServer) GetSchedule(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	sched, err := s.repo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, sched)
}

type patchScheduleRequest struct {
	Prompt   *string `json:"prompt,omitempty"`
	CronExpr *string `json:"cron_expr,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

// UpdateSchedule (PATCH /schedules/:id).
func (s *ScheduleServer) UpdateSchedule(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	sched, err := s.repo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	var req patchScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	if req.Prompt != nil {
		sched.Prompt = *req.Prompt
	}
	if req.CronExpr != nil {
		if err := s.sch.ValidateCron(*req.CronExpr); err != nil {
			respondValidationError(c, err.Error())
			return
		}
		sched.CronExpr = *req.CronExpr
		sched.NextFireAt = s.sch.NextFire(*req.CronExpr, sched.NextFireAt)
		sched.LastError = ""
	}
	if req.Enabled != nil {
		sched.Enabled = *req.Enabled
	}
	if err := s.repo.Update(sched); err != nil {
		respondInternal(c, "failed to update schedule", err)
		return
	}
	c.JSON(http.StatusOK, sched)
}

// DeleteSchedule (DELETE /schedules/:id).
func (s *ScheduleServer) DeleteSchedule(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}
	if err := s.repo.Delete(id); err != nil {
		respondInternal(c, "failed to delete schedule", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}
