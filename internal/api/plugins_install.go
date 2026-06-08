// Install + uninstall lifecycle endpoints for marketplace plugins
// (ADR 0002 §2, lifecycle-07). Lives in its own file because the
// install path pulls in bundle parsing, signature verification, the
// on-disk store, and the wasmhost loader — keeping it apart from the
// connection-management endpoints in plugins.go preserves the smaller
// file's readability.
//
// Install flow:
//
//  1. Source the bundle bytes (URL fetch or multipart upload).
//  2. bundle.Open — structural validation.
//  3. signing.Verify — chain check (v1 deny if no verifier configured
//     for marketplace bundles; dev path uses a separate handler).
//  4. wasmhost.Load — compile and instantiate the WASM module.
//  5. store.Install — persist bundle to disk atomically.
//  6. registry.Register — wire into the live runtime.
//  7. plugin_state Upsert — record installed=true, enabled=false.
//
// On any failure past step 1, partial state is rolled back: the
// wasmhost module is closed, the on-disk dir is removed, the registry
// is unregistered. The state row is only written on full success so
// /plugins/:id/state is the single source of truth for "is this
// plugin actually here."

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/plugins/signing"
	"go.klarlabs.de/nomi/internal/plugins/store"
	"go.klarlabs.de/nomi/internal/plugins/update"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/plugins/wasmplugin"
	"go.klarlabs.de/nomi/internal/secrets"
)

// InstallDependencies bundles the install-path collaborators the
// PluginServer needs. Optional — the server still serves the read +
// connection endpoints when these are nil; install/uninstall just
// return 503 in that case.
type InstallDependencies struct {
	// Store persists bundles under ~/.nomi/plugins/<id>/.
	Store *store.Store
	// Verifier walks the publisher → root chain. nil disables marketplace
	// install (dev installs go through a separate endpoint when that
	// lands in lifecycle-08).
	Verifier *signing.Verifier
	// Loader compiles bundle .wasm bytes into wasmhost.Module instances.
	Loader *wasmhost.Loader
	// HTTPFetch is the function called when the install source is a URL.
	// Defaults to a 30s-timeout http.Client when nil. Tests inject a
	// stub that returns canned bytes so the handler is exercisable
	// without a network.
	HTTPFetch func(ctx context.Context, url string) ([]byte, error)
	// CatalogProvider supplies the parsed marketplace catalog for the
	// GET /plugins/marketplace endpoint. Optional — when nil the
	// endpoint returns 503. Lives on the same dependency struct
	// because the marketplace-tier surface is logically one feature.
	CatalogProvider func(ctx context.Context) (*hub.Catalog, error)
	// Updater backs POST /plugins/:id/update (lifecycle-10). Wired
	// when the daemon has marketplace deps. nil leaves the endpoint
	// returning 503.
	Updater func(ctx context.Context, pluginID string) (*domain.PluginState, error)
}

// AttachInstall wires the install/uninstall capability into the
// PluginServer. Call once at boot after NewPluginServer. nil deps
// leave the install endpoints disabled (they return 503).
func (s *PluginServer) AttachInstall(deps InstallDependencies) {
	s.install = &deps
	if s.install.HTTPFetch == nil {
		s.install.HTTPFetch = defaultHTTPFetch
	}
}

// InstallPluginRequest is the JSON body shape for URL-source installs.
// Multipart uploads use the standard `bundle` form field instead.
type InstallPluginRequest struct {
	URL string `json:"url"`
}

// InstallPlugin handles POST /plugins/install. Sources bundle bytes
// from one of two paths and runs the full install pipeline. Returns
// the installed plugin's manifest + state on success.
func (s *PluginServer) InstallPlugin(c *gin.Context) {
	if !s.installReady(c) {
		return
	}

	bundleBytes, source, err := s.sourceBundle(c)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}

	b, err := bundle.Open(strings.NewReader(string(bundleBytes)))
	if err != nil {
		respondValidationError(c, fmt.Sprintf("invalid bundle: %v", err))
		return
	}

	if err := s.install.Verifier.Verify(b); err != nil {
		respondValidationError(c, fmt.Sprintf("signature verification failed: %v", err))
		return
	}

	// Refuse reinstall over an existing plugin (system or marketplace).
	if _, err := s.registry.Get(b.Manifest.ID); err == nil {
		respondError(c, http.StatusConflict, &domain.UserError{Code: "already_installed", Title: "Already Installed", Message: fmt.Sprintf("plugin %q already installed", b.Manifest.ID)})
		return
	}

	mod, err := s.install.Loader.Load(c.Request.Context(), b.Manifest.ID, b.WASM)
	if err != nil {
		respondValidationError(c, fmt.Sprintf("wasm load failed: %v", err))
		return
	}
	rollback := func() { _ = mod.Close(c.Request.Context()) }

	if err := s.install.Store.Install(b); err != nil {
		rollback()
		respondInternal(c, "store install failed", err)
		return
	}
	rollback = func() {
		_ = mod.Close(c.Request.Context())
		_ = s.install.Store.Remove(b.Manifest.ID)
	}

	plug := wasmplugin.New(b.Manifest, mod, nil)
	if err := s.registry.Register(plug); err != nil {
		rollback()
		respondInternal(c, "plugin registration failed", err)
		return
	}
	rollback = func() {
		_ = s.registry.Unregister(b.Manifest.ID)
		_ = mod.Close(c.Request.Context())
		_ = s.install.Store.Remove(b.Manifest.ID)
	}

	st := &domain.PluginState{
		PluginID:             b.Manifest.ID,
		Distribution:         domain.PluginDistributionMarketplace,
		Installed:            true,
		Enabled:              false,
		Version:              b.Manifest.Version,
		SourceURL:            source,
		SignatureFingerprint: b.Publisher.KeyFingerprint,
		InstalledAt:          time.Now().UTC(),
	}
	if s.state != nil {
		if err := s.state.Upsert(st); err != nil {
			rollback()
			respondInternal(c, "failed to persist plugin state", err)
			return
		}
	}

	c.JSON(http.StatusCreated, PluginResponse{
		Manifest:    b.Manifest,
		Status:      plug.Status(),
		State:       st,
		Connections: []ConnectionResponse{}, // fresh install — no connections yet
	})
}

// UninstallPlugin handles DELETE /plugins/:id?cascade=true|false.
// Default cascade=false preserves plugin_connections + bindings +
// secrets so a later reinstall reattaches them; cascade=true wipes
// everything. System plugins refuse with 409.
func (s *PluginServer) UninstallPlugin(c *gin.Context) {
	if !s.installReady(c) {
		return
	}
	id := c.Param("id")

	// Look up state first — system plugins refuse uninstall regardless of
	// whether they're currently registered.
	if s.state != nil {
		st, err := s.state.Get(id)
		if err == nil && st.Distribution == domain.PluginDistributionSystem {
			respondError(c, http.StatusConflict, &domain.UserError{Code: "system_plugin_refused", Title: "Uninstall Refused", Message: "system plugins cannot be uninstalled"})
			return
		}
	}

	cascade := c.Query("cascade") == "true"

	// Stop + unregister the live plugin if present. Tolerate
	// "not registered" — the plugin may have failed to load on boot
	// and we still want to clean the on-disk artifacts.
	if p, err := s.registry.Get(id); err == nil {
		_ = p.Stop()
		_ = s.registry.Unregister(id)
	}

	if err := s.install.Store.Remove(id); err != nil {
		respondInternal(c, "failed to remove plugin from store", err)
		return
	}

	if cascade {
		// Delete in dependency order: bindings reference connections;
		// connections reference plugin_id. The schema cascades on FK
		// for bindings → connections, so deleting the connections is
		// enough on the binding side.
		conns, err := s.connections.ListByPlugin(id)
		if err != nil {
			respondInternal(c, "failed to list plugin connections", err)
			return
		}
		for _, conn := range conns {
			s.deleteConnectionSecrets(id, conn.ID, conn.CredentialRefs)
			if err := s.connections.Delete(conn.ID); err != nil {
				respondInternal(c, fmt.Sprintf("failed to delete connection %s", conn.ID), err)
				return
			}
		}
		if s.state != nil {
			_ = s.state.Delete(id)
		}
	} else if s.state != nil {
		// Non-cascade: keep the row but flip installed=false so the
		// UI shows "not installed" while state row is the receipt
		// that connections still exist for reattach.
		st, err := s.state.Get(id)
		if err == nil {
			st.Installed = false
			st.Enabled = false
			_ = s.state.Upsert(st)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "uninstalled",
		"cascade": cascade,
	})
}

// installReady short-circuits the handler with 503 when the install
// dependencies haven't been wired. Centralized so both InstallPlugin
// and UninstallPlugin share the same error shape.
func (s *PluginServer) installReady(c *gin.Context) bool {
	if s.install == nil || s.install.Store == nil || s.install.Verifier == nil || s.install.Loader == nil {
		respondError(c, http.StatusServiceUnavailable, &domain.UserError{Code: "service_unavailable", Title: "Service Unavailable", Message: "install/uninstall not configured"})
		return false
	}
	return true
}

// MarketplaceCatalog handles GET /plugins/marketplace. Returns the
// most recent successfully-fetched + verified NomiHub catalog. Only
// available when the daemon was constructed with a CatalogProvider
// (which itself only fires up when NOMI_MARKETPLACE_ROOT_KEY is set).
func (s *PluginServer) MarketplaceCatalog(c *gin.Context) {
	if s.install == nil || s.install.CatalogProvider == nil {
		respondError(c, http.StatusServiceUnavailable, &domain.UserError{Code: "service_unavailable", Title: "Service Unavailable", Message: "marketplace catalog not configured"})
		return
	}
	cat, err := s.install.CatalogProvider(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, cat)
}

// UpdatePlugin handles POST /plugins/:id/update. Synchronous "update
// now" path: fetches the latest bundle from the catalog, runs the
// full verify + atomic swap. Returns the new plugin_state on success.
// Status code mapping follows the lifecycle-10 sentinel errors:
// 404 (not installed), 409 (system tier or already at latest), 502
// (catalog/download problem), 400 (bundle/signature problem),
// 500 for anything else.
func (s *PluginServer) UpdatePlugin(c *gin.Context) {
	if s.install == nil || s.install.Updater == nil {
		respondError(c, http.StatusServiceUnavailable, &domain.UserError{Code: "service_unavailable", Title: "Service Unavailable", Message: "updates not configured"})
		return
	}
	id := c.Param("id")
	st, err := s.install.Updater(c.Request.Context(), id)
	if err != nil {
		respondError(c, updateErrorStatus(err), err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// updateErrorStatus maps update package sentinels to HTTP codes the
// frontend can branch on. Centralized so the dialog UI doesn't need
// to string-match. The wrapped error retains the original cause so
// callers can also use errors.Is when needed.
func updateErrorStatus(err error) int {
	switch {
	case errors.Is(err, update.ErrPluginNotInstalled):
		return http.StatusNotFound
	case errors.Is(err, update.ErrSystemPluginRefused),
		errors.Is(err, update.ErrAlreadyAtLatest):
		return http.StatusConflict
	case errors.Is(err, update.ErrNoCatalogEntry):
		return http.StatusNotFound
	case errors.Is(err, update.ErrBundleHashMismatch):
		return http.StatusBadRequest
	case errors.Is(err, signing.ErrBundleSignatureBad),
		errors.Is(err, signing.ErrPublisherKeyExpired),
		errors.Is(err, signing.ErrPublisherKeyMissing),
		errors.Is(err, signing.ErrRootSignatureInvalid),
		errors.Is(err, signing.ErrRootSignatureMissing):
		return http.StatusBadRequest
	case errors.Is(err, hub.ErrCatalogFetchFailed):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// sourceBundle reads the bundle bytes from whichever input the request
// used. The dispatch is on Content-Type — JSON means URL, anything
// multipart means upload. Returns the bytes plus the source string
// recorded as plugin_state.source_url ("upload" for multipart, the URL
// itself for fetched).
func (s *PluginServer) sourceBundle(c *gin.Context) ([]byte, string, error) {
	const maxBundle = 64 * 1024 * 1024

	ct := c.GetHeader("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		var req InstallPluginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			return nil, "", fmt.Errorf("invalid JSON body: %w", err)
		}
		if req.URL == "" {
			return nil, "", errors.New("url is required")
		}
		body, err := s.install.HTTPFetch(c.Request.Context(), req.URL)
		if err != nil {
			return nil, "", fmt.Errorf("fetch %s: %w", req.URL, err)
		}
		return body, req.URL, nil
	case strings.HasPrefix(ct, "multipart/form-data"):
		header, err := c.FormFile("bundle")
		if err != nil {
			return nil, "", fmt.Errorf("multipart bundle field missing: %w", err)
		}
		if header.Size > maxBundle {
			return nil, "", fmt.Errorf("bundle exceeds %d bytes", maxBundle)
		}
		f, err := header.Open()
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = f.Close() }()
		body, err := io.ReadAll(io.LimitReader(f, maxBundle+1))
		if err != nil {
			return nil, "", err
		}
		if int64(len(body)) > maxBundle {
			return nil, "", fmt.Errorf("bundle exceeds %d bytes", maxBundle)
		}
		return body, "upload", nil
	default:
		return nil, "", fmt.Errorf("unsupported Content-Type %q (want application/json or multipart/form-data)", ct)
	}
}

// deleteConnectionSecrets best-effort wipes the keyring entries that
// stored this connection's credentials. Failure is logged-and-swallow
// rather than fatal: the user explicitly chose cascade, and a stuck
// keyring shouldn't trap them in a half-uninstalled state.
func (s *PluginServer) deleteConnectionSecrets(pluginID, connID string, refs map[string]string) {
	if s.secrets == nil {
		return
	}
	for _, ref := range refs {
		if !secrets.IsReference(ref) {
			continue
		}
		key, ok := secrets.KeyFromReference(ref)
		if !ok || key == "" {
			continue
		}
		_ = s.secrets.Delete(key)
		_ = pluginID
		_ = connID
	}
}

// defaultHTTPFetch is the production URL fetcher: a 30s-timeout HTTP
// GET that bounds response size at 64 MiB. Returned errors quote the
// status code so the dialog can surface "404 from NomiHub" rather
// than a vague "fetch failed."
func defaultHTTPFetch(ctx context.Context, url string) ([]byte, error) {
	const maxBundle = 64 * 1024 * 1024
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBundle+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBundle {
		return nil, fmt.Errorf("bundle exceeds %d bytes", maxBundle)
	}
	return body, nil
}

// _ ensures the plugins import is not flagged unused when the package
// is built without the wasmplugin path being touched (e.g. when
// install deps aren't wired). Costs nothing and keeps refactor noise
// low.
var _ = plugins.Plugin(nil)
