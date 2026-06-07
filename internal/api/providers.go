package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/llm"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// ProviderServer handles provider profile endpoints
type ProviderServer struct {
	profileRepo     *db.ProviderProfileRepository
	assistantRepo   *db.AssistantRepository
	settingsRepo    *db.GlobalSettingsRepository
	appSettingsRepo *db.AppSettingsRepository
	secrets         secrets.Store
}

// NewProviderServer creates a new provider server. The secrets store is used
// to stash API keys supplied via the UI; the DB column only ever stores a
// secret:// reference.
func NewProviderServer(database *db.DB, secretStore secrets.Store) *ProviderServer {
	return &ProviderServer{
		profileRepo:     db.NewProviderProfileRepository(database),
		assistantRepo:   db.NewAssistantRepository(database),
		settingsRepo:    db.NewGlobalSettingsRepository(database),
		appSettingsRepo: db.NewAppSettingsRepository(database),
		secrets:         secretStore,
	}
}

// stashProviderSecret converts a plaintext api-key input into a secret://
// reference for storage. If the value is already a reference or empty, it is
// returned unchanged.
func (s *ProviderServer) stashProviderSecret(profileID, raw string) (string, error) {
	if raw == "" || secrets.IsReference(raw) {
		return raw, nil
	}
	if s.secrets == nil {
		return raw, nil // no store configured; leave as plaintext for the legacy path
	}
	key := fmt.Sprintf("provider/%s/api_key", profileID)
	return secrets.StoreAsReference(s.secrets, key, raw)
}

// CreateProviderProfileRequest represents a request to create a provider profile
type CreateProviderProfileRequest struct {
	Name             string   `json:"name" binding:"required"`
	Type             string   `json:"type" binding:"required"` // "local" | "remote"
	Endpoint         string   `json:"endpoint,omitempty"`
	ModelIDs         []string `json:"model_ids" binding:"required"`
	EmbeddingModelID string   `json:"embedding_model_id,omitempty"`
	SecretRef        string   `json:"secret_ref,omitempty"`
	Enabled          bool     `json:"enabled"`
}

// CreateProviderProfile creates a new provider profile
func (s *ProviderServer) CreateProviderProfile(c *gin.Context) {
	var req CreateProviderProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	endpoint, err := llm.NormalizeEndpoint(req.Endpoint)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}

	id := uuid.New().String()
	secretRef, err := s.stashProviderSecret(id, req.SecretRef)
	if err != nil {
		respondInternal(c, "failed to stash secret", err)
		return
	}

	profile := &domain.ProviderProfile{
		ID:               id,
		Name:             req.Name,
		Type:             req.Type,
		Endpoint:         endpoint,
		ModelIDs:         req.ModelIDs,
		EmbeddingModelID: req.EmbeddingModelID,
		SecretRef:        secretRef,
		Enabled:          req.Enabled,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}

	if err := s.profileRepo.Create(profile); err != nil {
		respondInternal(c, "failed to create provider profile", err)
		return
	}

	c.JSON(http.StatusCreated, toProviderView(profile))
}

// providerView is the wire form of a ProviderProfile with the SecretRef
// field scrubbed so the UI never receives the secret:// URI or any fallback
// plaintext that predates migration.
type providerView struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Type             string    `json:"type"`
	Endpoint         string    `json:"endpoint,omitempty"`
	ModelIDs         []string  `json:"model_ids"`
	EmbeddingModelID string    `json:"embedding_model_id,omitempty"`
	SecretConfigured bool      `json:"secret_configured"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func toProviderView(p *domain.ProviderProfile) providerView {
	return providerView{
		ID:               p.ID,
		Name:             p.Name,
		Type:             p.Type,
		Endpoint:         p.Endpoint,
		ModelIDs:         p.ModelIDs,
		EmbeddingModelID: p.EmbeddingModelID,
		SecretConfigured: p.SecretRef != "",
		Enabled:          p.Enabled,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
	}
}

// GetProviderProfile retrieves a provider profile by ID
func (s *ProviderServer) GetProviderProfile(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	profile, err := s.profileRepo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, toProviderView(profile))
}

// ListProviderProfiles lists all provider profiles
func (s *ProviderServer) ListProviderProfiles(c *gin.Context) {
	profiles, err := s.profileRepo.List()
	if err != nil {
		respondInternal(c, "failed to list provider profiles", err)
		return
	}

	views := make([]providerView, 0, len(profiles))
	for _, p := range profiles {
		views = append(views, toProviderView(p))
	}
	c.JSON(http.StatusOK, gin.H{"profiles": views})
}

// UpdateProviderProfile updates a provider profile
func (s *ProviderServer) UpdateProviderProfile(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		respondValidationError(c, "id is required")
		return
	}

	var req CreateProviderProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	profile, err := s.profileRepo.GetByID(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	// Omitting secret_ref from an update means "keep the existing secret."
	// Sending a new plaintext value replaces it. Sending the same reference
	// URI back is a no-op. Callers who want to clear a secret should delete
	// the profile and recreate it — an explicit "rotate to nothing" flow
	// isn't worth a sentinel on the API here.
	if req.SecretRef != "" {
		secretRef, err := s.stashProviderSecret(profile.ID, req.SecretRef)
		if err != nil {
			respondInternal(c, "failed to stash secret", err)
			return
		}
		profile.SecretRef = secretRef
	}

	endpoint, err := llm.NormalizeEndpoint(req.Endpoint)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}

	profile.Name = req.Name
	profile.Type = req.Type
	profile.Endpoint = endpoint
	profile.ModelIDs = req.ModelIDs
	profile.EmbeddingModelID = req.EmbeddingModelID
	profile.Enabled = req.Enabled

	if err := s.profileRepo.Update(profile); err != nil {
		respondInternal(c, "failed to update provider profile", err)
		return
	}

	c.JSON(http.StatusOK, toProviderView(profile))
}

// ProbeProviderRequest represents an unsaved provider configuration the
// UI wants to verify before persisting. Mirrors CreateProviderProfileRequest
// minus the SecretRef stash + uuid generation; we treat the input as a
// throwaway snapshot.
type ProbeProviderRequest struct {
	Endpoint string   `json:"endpoint"`
	APIKey   string   `json:"api_key,omitempty"`
	ModelIDs []string `json:"model_ids,omitempty"`
}

// ProbeProviderResponse summarises whether a provider is reachable and
// which of the requested model IDs the provider claims to serve.
type ProbeProviderResponse struct {
	Reachable        bool     `json:"reachable"`
	Endpoint         string   `json:"endpoint"`
	ModelsAvailable  []string `json:"models_available,omitempty"`
	MissingRequested []string `json:"missing_requested,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// ProbeProvider attempts a GET <endpoint>/models against the configured
// (already-normalized) endpoint and reports whether the daemon would be
// able to talk to it. Designed to run BEFORE save so the user catches
// "model name typo" or "endpoint not reachable" without first triggering
// a chat that 404s. The probe never persists anything.
func (s *ProviderServer) ProbeProvider(c *gin.Context) {
	var req ProbeProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	endpoint, err := llm.NormalizeEndpoint(req.Endpoint)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	resp := llm.Probe(c.Request.Context(), endpoint, req.APIKey, req.ModelIDs)
	c.JSON(http.StatusOK, ProbeProviderResponse{
		Reachable:        resp.Reachable,
		Endpoint:         endpoint,
		ModelsAvailable:  resp.ModelsAvailable,
		MissingRequested: resp.MissingRequested,
		Error:            resp.Error,
	})
}

// DeleteProviderProfile deletes a provider profile
func (s *ProviderServer) DeleteProviderProfile(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}

	if err := s.profileRepo.Delete(id); err != nil {
		respondInternal(c, "failed to delete provider profile", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// GetLLMDefaultSettings returns the default LLM provider and model
func (s *ProviderServer) GetLLMDefaultSettings(c *gin.Context) {
	providerID, modelID := s.settingsRepo.GetLLMDefault()
	c.JSON(http.StatusOK, gin.H{
		"provider_id": providerID,
		"model_id":    modelID,
	})
}

// SetLLMDefaultSettingsRequest represents a request to set default LLM settings
type SetLLMDefaultSettingsRequest struct {
	ProviderID string `json:"provider_id" binding:"required"`
	ModelID    string `json:"model_id" binding:"required"`
}

// SetLLMDefaultSettings sets the default LLM provider and model
func (s *ProviderServer) SetLLMDefaultSettings(c *gin.Context) {
	var req SetLLMDefaultSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	if err := s.settingsRepo.SetLLMDefault(req.ProviderID, req.ModelID); err != nil {
		respondInternal(c, "failed to set LLM default", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"provider_id": req.ProviderID,
		"model_id":    req.ModelID,
	})
}

type onboardingCompleteResponse struct {
	Complete bool `json:"complete"`
}

type setOnboardingCompleteRequest struct {
	Complete bool `json:"complete"`
}

type safetyProfileResponse struct {
	Profile string `json:"profile"`
}

type setSafetyProfileRequest struct {
	Profile string `json:"profile" binding:"required"`
}

// GetOnboardingComplete returns whether first-run onboarding should be skipped.
func (s *ProviderServer) GetOnboardingComplete(c *gin.Context) {
	if value, err := s.appSettingsRepo.Get("onboarding.complete"); err == nil {
		c.JSON(http.StatusOK, onboardingCompleteResponse{Complete: value == "true"})
		return
	}

	assistants, err := s.assistantRepo.List(1, 0)
	if err != nil {
		respondInternal(c, "failed to list assistants for onboarding check", err)
		return
	}
	profiles, err := s.profileRepo.List()
	if err != nil {
		respondInternal(c, "failed to list provider profiles for onboarding check", err)
		return
	}

	complete := len(assistants) > 0 && len(profiles) > 0
	c.JSON(http.StatusOK, onboardingCompleteResponse{Complete: complete})
}

// SetOnboardingComplete explicitly sets onboarding completion state.
func (s *ProviderServer) SetOnboardingComplete(c *gin.Context) {
	var req setOnboardingCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	value := "false"
	if req.Complete {
		value = "true"
	}
	if err := s.appSettingsRepo.Set("onboarding.complete", value); err != nil {
		respondInternal(c, "failed to set onboarding complete", err)
		return
	}

	c.JSON(http.StatusOK, onboardingCompleteResponse{Complete: req.Complete})
}

// GetSafetyProfile returns the global safety profile used for new assistants.
func (s *ProviderServer) GetSafetyProfile(c *gin.Context) {
	profile := s.appSettingsRepo.GetOrDefault("safety_profile", permissions.DefaultSafetyProfile)
	c.JSON(http.StatusOK, safetyProfileResponse{Profile: profile})
}

// SetSafetyProfile updates the global safety profile used for new assistants.
func (s *ProviderServer) SetSafetyProfile(c *gin.Context) {
	var req setSafetyProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	if !permissions.IsValidSafetyProfile(req.Profile) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid profile: must be one of " + strings.Join(permissions.ValidSafetyProfiles(), ", "),
		})
		return
	}

	if err := s.appSettingsRepo.Set("safety_profile", req.Profile); err != nil {
		respondInternal(c, "failed to set safety profile", err)
		return
	}

	c.JSON(http.StatusOK, safetyProfileResponse{Profile: req.Profile})
}
