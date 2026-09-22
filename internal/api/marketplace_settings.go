package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// MarketplaceCatalogServer exposes settings for the WASM marketplace
// catalog URL (Goose-style: point nomid at any signed index.json).
type MarketplaceCatalogServer struct {
	provider *hub.CachedProvider
	settings *db.AppSettingsRepository
}

// NewMarketplaceCatalogServer wires the cached hub provider + settings.
func NewMarketplaceCatalogServer(provider *hub.CachedProvider, settings *db.AppSettingsRepository) *MarketplaceCatalogServer {
	return &MarketplaceCatalogServer{provider: provider, settings: settings}
}

type marketplaceCatalogSettingsResponse struct {
	URL          string `json:"url"`
	EffectiveURL string `json:"effective_url"`
	DefaultURL   string `json:"default_url"`
	EntryCount   int    `json:"entry_count"`
	FetchedAt    string `json:"fetched_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	Stale        bool   `json:"stale"`
	Configured   bool   `json:"configured"` // false when marketplace root key missing
}

type setMarketplaceCatalogRequest struct {
	URL string `json:"url"`
}

func marketplaceSettingsFrom(p *hub.CachedProvider, configured bool) marketplaceCatalogSettingsResponse {
	resp := marketplaceCatalogSettingsResponse{
		DefaultURL: hub.DefaultCatalogURL,
		Configured: configured,
	}
	if p == nil {
		resp.EffectiveURL = hub.DefaultCatalogURL
		return resp
	}
	st := p.Status()
	resp.URL = st.URL
	resp.EffectiveURL = st.EffectiveURL
	resp.EntryCount = st.EntryCount
	resp.LastError = st.LastError
	resp.Stale = st.Stale
	if !st.FetchedAt.IsZero() {
		resp.FetchedAt = st.FetchedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return resp
}

// GetMarketplaceCatalogSettings returns the configured catalog URL + status.
func (s *MarketplaceCatalogServer) GetMarketplaceCatalogSettings(c *gin.Context) {
	configured := s.provider != nil
	if s.settings != nil && s.provider != nil {
		url := strings.TrimSpace(s.settings.GetOrDefault(hub.SettingKey, ""))
		if s.provider.URL() != url {
			_ = s.provider.SetURL(url)
		}
	}
	c.JSON(http.StatusOK, marketplaceSettingsFrom(s.provider, configured))
}

// SetMarketplaceCatalogSettings persists the catalog URL and clears cache.
// Empty url resets to the default hub.nomi.ai index.
func (s *MarketplaceCatalogServer) SetMarketplaceCatalogSettings(c *gin.Context) {
	if s.provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "marketplace not configured — set NOMI_MARKETPLACE_ROOT_KEY",
		})
		return
	}
	var req setMarketplaceCatalogRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	url := strings.TrimSpace(req.URL)
	if url != "" {
		if err := hub.ValidateCatalogURL(url); err != nil {
			respondValidationError(c, err.Error())
			return
		}
	}
	if s.settings != nil {
		if err := s.settings.Set(hub.SettingKey, url); err != nil {
			respondInternal(c, "failed to save marketplace catalog url", err)
			return
		}
	}
	if err := s.provider.SetURL(url); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	// Best-effort warm; failures still return 200 with last_error.
	_, _ = s.provider.Refresh(c.Request.Context())
	c.JSON(http.StatusOK, marketplaceSettingsFrom(s.provider, true))
}

// RefreshMarketplaceCatalog force-reloads the remote index.
func (s *MarketplaceCatalogServer) RefreshMarketplaceCatalog(c *gin.Context) {
	if s.provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "marketplace not configured — set NOMI_MARKETPLACE_ROOT_KEY",
		})
		return
	}
	cat, err := s.provider.Refresh(c.Request.Context())
	resp := marketplaceSettingsFrom(s.provider, true)
	if err != nil && (cat == nil || len(cat.Entries) == 0) {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   err.Error(),
			"catalog": resp,
		})
		return
	}
	c.JSON(http.StatusOK, resp)
}
