package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"go.klarlabs.de/nomi/internal/llm"
	"go.klarlabs.de/nomi/internal/scheduler"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// ScheduleServer handles REST CRUD for schedules.
type ScheduleServer struct {
	repo *db.ScheduleRepository
	sch  *scheduler.Scheduler
	llm  *llm.Resolver
}

// NewScheduleServer wires the schedule REST handlers. The scheduler is
// consulted for cron validation + next-fire calculation; the repository
// owns persistence. llmResolver is optional — when nil, the NL
// translation endpoint returns 503 but cron CRUD still works.
func NewScheduleServer(repo *db.ScheduleRepository, sch *scheduler.Scheduler, llmResolver *llm.Resolver) *ScheduleServer {
	return &ScheduleServer{repo: repo, sch: sch, llm: llmResolver}
}

type createScheduleRequest struct {
	AssistantID string `json:"assistant_id" binding:"required"`
	Prompt      string `json:"prompt" binding:"required"`
	CronExpr    string `json:"cron_expr" binding:"required"`
	NLPhrase    string `json:"nl_phrase,omitempty"`
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
	sched.NLPhrase = req.NLPhrase
	if req.Enabled != nil {
		sched.Enabled = *req.Enabled
	}
	if err := s.repo.Create(sched); err != nil {
		respondInternal(c, "failed to create schedule", err)
		return
	}
	c.JSON(http.StatusCreated, sched)
}

// translateRequest carries the natural-language phrase the user typed.
type translateRequest struct {
	Phrase string `json:"phrase" binding:"required"`
}

// TranslateNL (POST /schedules/translate). Converts a natural-language
// phrase to a cron expression via the configured LLM. The returned
// payload includes a Valid flag the UI can check before calling
// CreateSchedule with the parsed expression.
func (s *ScheduleServer) TranslateNL(c *gin.Context) {
	if s.llm == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no LLM provider configured; configure one in Settings → AI Providers to enable natural-language schedules"})
		return
	}
	var req translateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	client, _, err := s.llm.DefaultClient()
	if err != nil || client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "default LLM provider is unavailable"})
		return
	}
	result, err := s.sch.TranslateNL(c.Request.Context(), client, req.Phrase)
	if err != nil {
		respondInternal(c, "translation failed", err)
		return
	}
	c.JSON(http.StatusOK, result)
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
