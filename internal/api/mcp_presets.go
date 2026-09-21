package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/mcpcatalog"
	"go.klarlabs.de/nomi/internal/storage/db"
)

const mcpPresetCatalogURLKey = mcpcatalog.SettingKey

// MCPPresetServer exposes remote MCP preset catalog endpoints.
type MCPPresetServer struct {
	client   *mcpcatalog.Client
	settings *db.AppSettingsRepository
}

// NewMCPPresetServer wires the catalog client + settings persistence.
func NewMCPPresetServer(client *mcpcatalog.Client, settings *db.AppSettingsRepository) *MCPPresetServer {
	return &MCPPresetServer{client: client, settings: settings}
}

type mcpPresetsResponse struct {
	Presets []mcpcatalog.Preset `json:"presets"`
	Catalog mcpcatalog.Status   `json:"catalog"`
}

// ListMCPPresets returns remote catalog presets (empty when no URL set).
// Built-in presets remain client-side; the UI merges both lists.
func (s *MCPPresetServer) ListMCPPresets(c *gin.Context) {
	if s.client == nil {
		c.JSON(http.StatusOK, mcpPresetsResponse{
			Presets: []mcpcatalog.Preset{},
			Catalog: mcpcatalog.Status{},
		})
		return
	}
	presets, err := s.client.Presets(c.Request.Context())
	st := s.client.Status()
	if presets == nil {
		presets = []mcpcatalog.Preset{}
	}
	if err != nil && len(presets) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   err.Error(),
			"presets": []mcpcatalog.Preset{},
			"catalog": st,
		})
		return
	}
	// Soft-fail: serve cache + surface last_error on catalog status.
	c.JSON(http.StatusOK, mcpPresetsResponse{Presets: presets, Catalog: st})
}

// RefreshMCPPresets force-reloads the remote catalog.
func (s *MCPPresetServer) RefreshMCPPresets(c *gin.Context) {
	if s.client == nil {
		respondValidationError(c, "mcp preset catalog not configured")
		return
	}
	presets, err := s.client.Refresh(c.Request.Context())
	st := s.client.Status()
	if presets == nil {
		presets = []mcpcatalog.Preset{}
	}
	if err != nil && len(presets) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   err.Error(),
			"presets": []mcpcatalog.Preset{},
			"catalog": st,
		})
		return
	}
	c.JSON(http.StatusOK, mcpPresetsResponse{Presets: presets, Catalog: st})
}

type mcpCatalogSettingsResponse struct {
	URL             string `json:"url"`
	ExampleGooseURL string `json:"example_goose_url"`
	PresetCount     int    `json:"preset_count"`
	FetchedAt       string `json:"fetched_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	Stale           bool   `json:"stale"`
}

type setMCPCatalogRequest struct {
	URL string `json:"url"`
}

func mcpCatalogSettingsFrom(client *mcpcatalog.Client, url string) mcpCatalogSettingsResponse {
	resp := mcpCatalogSettingsResponse{
		URL:             url,
		ExampleGooseURL: mcpcatalog.DefaultGooseCatalogURL,
	}
	if client == nil {
		return resp
	}
	st := client.Status()
	resp.URL = st.URL
	resp.PresetCount = st.PresetCount
	resp.LastError = st.LastError
	resp.Stale = st.Stale
	if !st.FetchedAt.IsZero() {
		resp.FetchedAt = st.FetchedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return resp
}

// GetMCPPresetCatalog returns the configured remote catalog URL + status.
func (s *MCPPresetServer) GetMCPPresetCatalog(c *gin.Context) {
	url := ""
	if s.settings != nil {
		url = strings.TrimSpace(s.settings.GetOrDefault(mcpPresetCatalogURLKey, ""))
	}
	if s.client != nil && s.client.URL() != url {
		_ = s.client.SetURL(url)
	}
	c.JSON(http.StatusOK, mcpCatalogSettingsFrom(s.client, url))
}

// SetMCPPresetCatalog persists the catalog URL and clears/refreshes cache.
// Empty url disables the remote catalog.
func (s *MCPPresetServer) SetMCPPresetCatalog(c *gin.Context) {
	var req setMCPCatalogRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	url := strings.TrimSpace(req.URL)
	if url != "" {
		if err := mcpcatalog.ValidateCatalogURL(url); err != nil {
			respondValidationError(c, err.Error())
			return
		}
	}
	if s.settings != nil {
		if err := s.settings.Set(mcpPresetCatalogURLKey, url); err != nil {
			respondInternal(c, "failed to save mcp catalog url", err)
			return
		}
	}
	if s.client != nil {
		if err := s.client.SetURL(url); err != nil {
			respondValidationError(c, err.Error())
			return
		}
		if url != "" {
			// Best-effort warm cache; failures still return 200 with last_error.
			_, _ = s.client.Refresh(c.Request.Context())
		}
	}
	c.JSON(http.StatusOK, mcpCatalogSettingsFrom(s.client, url))
}
