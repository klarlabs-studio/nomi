package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// ApprovalServer handles approval-related endpoints
type ApprovalServer struct {
	manager      *permissions.Manager
	settingsRepo *db.AppSettingsRepository
}

// NewApprovalServer creates a new approval server
func NewApprovalServer(manager *permissions.Manager, database *db.DB) *ApprovalServer {
	return &ApprovalServer{manager: manager, settingsRepo: db.NewAppSettingsRepository(database)}
}

// ListApprovals lists all approvals
func (s *ApprovalServer) ListApprovals(c *gin.Context) {
	approvals, err := s.manager.GetPending()
	if err != nil {
		respondInternal(c, "failed to list approvals", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"approvals": approvals})
}

// GetApproval retrieves a single approval by ID
func (s *ApprovalServer) GetApproval(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	approval, err := s.manager.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, approval)
}

// ResolveApproval resolves an approval request
type ResolveApprovalRequest struct {
	Approved bool `json:"approved"`
	Remember bool `json:"remember"`
}

type rememberedApprovalDecision struct {
	Approved      bool      `json:"approved"`
	ExpiresAt     time.Time `json:"expires_at"`
	SafetyProfile string    `json:"safety_profile"`
}

func (s *ApprovalServer) ResolveApproval(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	var req ResolveApprovalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	approval, err := s.manager.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	if err := s.manager.Resolve(c.Request.Context(), id, req.Approved); err != nil {
		respondInternal(c, "failed to resolve approval", err)
		return
	}

	if req.Remember {
		s.rememberChoice(approval, req.Approved)
	}

	c.JSON(http.StatusOK, gin.H{"status": "resolved", "approved": req.Approved, "remembered": req.Remember})
}

func (s *ApprovalServer) rememberChoice(approval *permissions.ApprovalRequest, approved bool) {
	if s.settingsRepo == nil || approval == nil || approval.Context == nil {
		return
	}
	assistantID, _ := approval.Context["assistant_id"].(string)
	inputSignature, _ := approval.Context["input_signature"].(string)
	if assistantID == "" || approval.Capability == "" || inputSignature == "" {
		return
	}
	ttlHours := approvalRememberTTLHours(s.settingsRepo)
	value, err := json.Marshal(rememberedApprovalDecision{
		Approved:      approved,
		ExpiresAt:     time.Now().UTC().Add(time.Duration(ttlHours) * time.Hour),
		SafetyProfile: s.settingsRepo.GetOrDefault("safety_profile", permissions.DefaultSafetyProfile),
	})
	if err != nil {
		return
	}
	_ = s.settingsRepo.Set(rememberedApprovalKey(assistantID, approval.Capability, inputSignature), string(value))
}

func rememberedApprovalKey(assistantID, capability, inputSignature string) string {
	return fmt.Sprintf("approval.remember.%s.%s.%s", assistantID, capability, inputSignature)
}

func approvalRememberTTLHours(repo *db.AppSettingsRepository) int {
	if repo == nil {
		return 24
	}
	raw := repo.GetOrDefault("approval_remember_ttl_hours", "24")
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return 24
	}
	return v
}
