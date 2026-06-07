package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginServer exposes REST endpoints for managing plugins and their
// Connections (ADR 0001). These are the endpoints the new Plugins tab
// in the UI consumes; the legacy /connectors/... surface stays for
// backward compatibility with the existing Connections tab until it
// gets retired in a follow-up cleanup task.
type PluginServer struct {
	registry    *plugins.Registry
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	state       *db.PluginStateRepository
	secrets     secrets.Store
	// install is populated by AttachInstall after construction. nil
	// means the marketplace install/uninstall surface returns 503
	// (the read + connection-management endpoints work either way).
	install *InstallDependencies
}

// NewPluginServer constructs the plugin endpoint handler.
func NewPluginServer(registry *plugins.Registry, conns *db.ConnectionRepository, binds *db.AssistantBindingRepository, state *db.PluginStateRepository, secretStore secrets.Store) *PluginServer {
	return &PluginServer{
		registry:    registry,
		connections: conns,
		bindings:    binds,
		state:       state,
		secrets:     secretStore,
	}
}

// PluginResponse is the per-plugin payload shape. We return the full
// manifest plus the connection list so the UI can render everything it
// needs in a single round-trip.
type PluginResponse struct {
	Manifest    plugins.PluginManifest `json:"manifest"`
	Status      plugins.PluginStatus   `json:"status"`
	State       *domain.PluginState    `json:"state,omitempty"`
	Connections []ConnectionResponse   `json:"connections"`
}

// ConnectionResponse redacts credential references — the frontend only
// needs to know whether a credential slot is populated, not the secret://
// URI itself. Matches the existing TelegramConfig redaction pattern.
//
// Health is populated when the owning plugin implements
// plugins.ConnectionHealthReporter; otherwise it's nil and the UI falls
// back to plugin-level status.
type ConnectionResponse struct {
	ID                    string                    `json:"id"`
	PluginID              string                    `json:"plugin_id"`
	Name                  string                    `json:"name"`
	Config                map[string]interface{}    `json:"config"`
	Credentials           map[string]bool           `json:"credentials"` // key → configured?
	Enabled               bool                      `json:"enabled"`
	Health                *plugins.ConnectionHealth `json:"health,omitempty"`
	WebhookURL            string                    `json:"webhook_url,omitempty"`
	WebhookEnabled        bool                      `json:"webhook_enabled"`
	WebhookEventAllowlist []string                  `json:"webhook_event_allowlist"`
	CreatedAt             time.Time                 `json:"created_at"`
	UpdatedAt             time.Time                 `json:"updated_at"`
}

// ListPlugins returns every registered plugin with its manifest, status,
// and current connection list. This single endpoint powers the Plugins
// tab's initial render.
func (s *PluginServer) ListPlugins(c *gin.Context) {
	pluginsList := s.registry.List()
	out := make([]PluginResponse, 0, len(pluginsList))
	for _, p := range pluginsList {
		manifest := p.Manifest()
		conns, err := s.connections.ListByPlugin(manifest.ID)
		if err != nil {
			respondInternal(c, "failed to list plugin connections", err)
			return
		}
		var state *domain.PluginState
		if s.state != nil {
			st, err := s.state.Get(manifest.ID)
			if err == nil {
				state = st
			}
		}
		out = append(out, PluginResponse{
			Manifest:    manifest,
			Status:      p.Status(),
			State:       state,
			Connections: annotateWithHealth(p, toConnectionResponses(conns)),
		})
	}
	c.JSON(http.StatusOK, gin.H{"plugins": out})
}

// annotateWithHealth fills in Health on each connection when the owning
// plugin implements ConnectionHealthReporter. Leaves Health nil otherwise;
// the UI falls back to plugin-level status.
func annotateWithHealth(p plugins.Plugin, conns []ConnectionResponse) []ConnectionResponse {
	reporter, ok := p.(plugins.ConnectionHealthReporter)
	if !ok {
		return conns
	}
	for i := range conns {
		if h, ok := reporter.ConnectionHealth(conns[i].ID); ok {
			h := h // copy to avoid aliasing the loop variable
			conns[i].Health = &h
		}
	}
	return conns
}

// GetPlugin returns one plugin by ID with its connections + state row.
// State is included so single-plugin views (the install dialog after
// install, the marketplace browser drill-in) can show enable/disable
// + distribution + available_version without a second API hop.
func (s *PluginServer) GetPlugin(c *gin.Context) {
	id := c.Param("id")
	p, err := s.registry.Get(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	manifest := p.Manifest()
	conns, err := s.connections.ListByPlugin(manifest.ID)
	if err != nil {
		respondInternal(c, "failed to list plugin connections", err)
		return
	}
	var state *domain.PluginState
	if s.state != nil {
		if st, err := s.state.Get(manifest.ID); err == nil {
			state = st
		}
	}
	c.JSON(http.StatusOK, PluginResponse{
		Manifest:    manifest,
		Status:      p.Status(),
		State:       state,
		Connections: annotateWithHealth(p, toConnectionResponses(conns)),
	})
}

// GetPluginState returns the lifecycle state row for a plugin (ADR 0002 §1).
func (s *PluginServer) GetPluginState(c *gin.Context) {
	id := c.Param("id")
	if s.state == nil {
		respondNotFound(c, "plugin state not configured")
		return
	}
	st, err := s.state.Get(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, st)
}

// UpdatePluginStateRequest carries the writable fields of plugin_state.
// v1 only exposes Enabled — distribution / installed / version are
// managed by the install/uninstall path (lifecycle-07).
type UpdatePluginStateRequest struct {
	Enabled      *bool    `json:"enabled,omitempty"`
	EnabledRoles []string `json:"enabled_roles,omitempty"`
}

// PatchPluginState applies enable/disable transitions. System plugins
// can be toggled like any other; the UI surfaces the toggle but warns
// users that disabling a system plugin removes the channel/tool
// surface they may depend on.
//
// Hot-reload: persisting the state row + calling Plugin.Start/Stop
// here means the toggle takes effect immediately — no daemon restart
// required. Errors during Start/Stop don't undo the persisted state
// (the user's intent is recorded; the next manual Refresh or daemon
// boot will retry the lifecycle).
func (s *PluginServer) PatchPluginState(c *gin.Context) {
	id := c.Param("id")
	if s.state == nil {
		respondInternal(c, "plugin state not configured", nil)
		return
	}
	plug, err := s.registry.Get(id)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	var req UpdatePluginStateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	if req.Enabled != nil {
		previous, _ := s.state.IsEnabled(id)
		if err := s.state.SetEnabled(id, *req.Enabled); err != nil {
			respondInternal(c, "failed to update plugin enabled state", err)
			return
		}
		// Reconcile the live plugin instance with the new state. We
		// only act on transitions (false→true, true→false); a
		// no-op patch with the same value as before doesn't restart
		// a healthy plugin.
		if previous != *req.Enabled {
			if *req.Enabled {
				if err := plug.Start(c.Request.Context()); err != nil {
					// Soft-fail: the state row is the user's intent;
					// surface the start error so the UI can show it.
					c.JSON(http.StatusOK, gin.H{
						"plugin_id":     id,
						"enabled":       true,
						"start_warning": err.Error(),
					})
					return
				}
			} else {
				_ = plug.Stop() // Stop is best-effort; persisted state already records the intent.
			}
		}
	}
	if req.EnabledRoles != nil {
		if err := s.state.SetEnabledRoles(id, req.EnabledRoles); err != nil {
			respondInternal(c, "failed to update plugin roles state", err)
			return
		}
	}
	st, err := s.state.Get(id)
	if err != nil {
		respondInternal(c, "failed to read plugin state", err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// CreateConnectionRequest is the payload for POST /plugins/:id/connections.
// Credentials arrive as plaintext (bot tokens, app passwords) and are
// stashed into secrets.Store; what lands in plugin_connections is the
// secret:// reference, not the plaintext.
type CreateConnectionRequest struct {
	Name        string            `json:"name"`
	Config      map[string]any    `json:"config"`
	Credentials map[string]string `json:"credentials"` // key → plaintext
	Enabled     bool              `json:"enabled"`
}

// CreateConnection persists a new plugin Connection. Pluggable per-plugin
// validation (e.g. "telegram requires bot_token") lives in the manifest's
// Requires.Credentials field, which the handler enforces here.
func (s *PluginServer) CreateConnection(c *gin.Context) {
	pluginID := c.Param("id")
	p, err := s.registry.Get(pluginID)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	manifest := p.Manifest()

	var req CreateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	if req.Name == "" {
		respondValidationError(c, "name is required")
		return
	}

	// Validate required credentials are present.
	for _, cred := range manifest.Requires.Credentials {
		if !cred.Required {
			continue
		}
		if req.Credentials[cred.Key] == "" {
			respondValidationError(c, fmt.Sprintf("credential %q is required", cred.Key))
			return
		}
	}

	connID := uuid.New().String()
	credRefs, err := s.stashCredentials(pluginID, connID, req.Credentials)
	if err != nil {
		respondInternal(c, "failed to stash credentials", err)
		return
	}

	if req.Config == nil {
		req.Config = map[string]any{}
	}
	if err := s.connections.Create(&domain.Connection{
		ID:             connID,
		PluginID:       pluginID,
		Name:           req.Name,
		Config:         req.Config,
		CredentialRefs: credRefs,
		Enabled:        req.Enabled,
	}); err != nil {
		respondInternal(c, "failed to create plugin connection", err)
		return
	}

	// Restart the plugin so it picks up the new connection. Failure here
	// is soft — the connection persisted, next daemon boot will activate it.
	if err := p.Stop(); err == nil {
		_ = p.Start(c.Request.Context())
	}

	created, _ := s.connections.GetByID(connID)
	c.JSON(http.StatusCreated, toConnectionResponse(created))
}

// UpdateConnectionRequest updates a connection in place. Credentials are
// optional — if absent, the existing refs are preserved.
type UpdateConnectionRequest struct {
	Name                  *string           `json:"name,omitempty"`
	Config                map[string]any    `json:"config,omitempty"`
	Credentials           map[string]string `json:"credentials,omitempty"` // plaintext
	Enabled               *bool             `json:"enabled,omitempty"`
	WebhookEnabled        *bool             `json:"webhook_enabled,omitempty"`
	WebhookEventAllowlist []string          `json:"webhook_event_allowlist,omitempty"`
}

// UpdateConnection modifies an existing connection.
func (s *PluginServer) UpdateConnection(c *gin.Context) {
	pluginID := c.Param("id")
	connID := c.Param("conn_id")

	existing, err := s.connections.GetByID(connID)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	if existing.PluginID != pluginID {
		respondValidationError(c, "connection does not belong to this plugin")
		return
	}

	var req UpdateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	if req.Name != nil {
		existing.Name = *req.Name
	}
	if req.Config != nil {
		existing.Config = req.Config
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.WebhookEnabled != nil {
		existing.WebhookEnabled = *req.WebhookEnabled
	}
	if req.WebhookEventAllowlist != nil {
		existing.WebhookEventAllowlist = req.WebhookEventAllowlist
	}
	if len(req.Credentials) > 0 {
		updates, err := s.stashCredentials(pluginID, connID, req.Credentials)
		if err != nil {
			respondInternal(c, "failed to stash credentials", err)
			return
		}
		if existing.CredentialRefs == nil {
			existing.CredentialRefs = map[string]string{}
		}
		for k, v := range updates {
			existing.CredentialRefs[k] = v
		}
	}

	if err := s.connections.Update(existing); err != nil {
		respondInternal(c, "failed to update plugin connection", err)
		return
	}

	// Restart plugin to pick up config changes.
	if p, err := s.registry.Get(pluginID); err == nil {
		_ = p.Stop()
		_ = p.Start(c.Request.Context())
	}

	updated, _ := s.connections.GetByID(connID)
	c.JSON(http.StatusOK, toConnectionResponse(updated))
}

// DeleteConnection removes a connection. Bindings cascade (FK).
func (s *PluginServer) DeleteConnection(c *gin.Context) {
	pluginID := c.Param("id")
	connID := c.Param("conn_id")

	existing, err := s.connections.GetByID(connID)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}
	if existing.PluginID != pluginID {
		respondValidationError(c, "connection does not belong to this plugin")
		return
	}
	if err := s.connections.Delete(connID); err != nil {
		respondInternal(c, "failed to delete plugin connection", err)
		return
	}
	// Restart plugin so it stops polling the deleted connection.
	if p, err := s.registry.Get(pluginID); err == nil {
		_ = p.Stop()
		_ = p.Start(c.Request.Context())
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// ListAssistantBindings returns the set of connection bindings for an
// assistant. Used by the Agent builder view (plugin-ui-02).
func (s *PluginServer) ListAssistantBindings(c *gin.Context) {
	assistantID := c.Param("id")
	list, err := s.bindings.ListByAssistant(assistantID)
	if err != nil {
		respondInternal(c, "failed to list assistant bindings", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"bindings": list})
}

// UpsertBindingRequest configures one binding between an assistant and
// a connection for a given role. Primary disambiguates when the
// assistant has N bindings for the same (plugin, role).
type UpsertBindingRequest struct {
	ConnectionID string             `json:"connection_id"`
	Role         domain.BindingRole `json:"role"`
	Enabled      bool               `json:"enabled"`
	IsPrimary    bool               `json:"is_primary"`
	Priority     int                `json:"priority"`
}

// UpsertAssistantBinding sets or updates one assistant→connection binding.
func (s *PluginServer) UpsertAssistantBinding(c *gin.Context) {
	assistantID := c.Param("id")
	var req UpsertBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}
	if !req.Role.IsValid() {
		respondValidationError(c, "invalid role")
		return
	}
	if err := s.bindings.Upsert(&domain.AssistantConnectionBinding{
		AssistantID:  assistantID,
		ConnectionID: req.ConnectionID,
		Role:         req.Role,
		Enabled:      req.Enabled,
		IsPrimary:    req.IsPrimary,
		Priority:     req.Priority,
	}); err != nil {
		respondInternal(c, "failed to upsert assistant binding", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "upserted"})
}

// DeleteAssistantBinding removes one assistant→connection binding.
func (s *PluginServer) DeleteAssistantBinding(c *gin.Context) {
	assistantID := c.Param("id")
	connectionID := c.Param("conn_id")
	role := domain.BindingRole(c.Param("role"))
	if !role.IsValid() {
		respondValidationError(c, "invalid role")
		return
	}
	if err := s.bindings.Delete(assistantID, connectionID, role); err != nil {
		respondInternal(c, "failed to delete assistant binding", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// --- helpers ---

// stashCredentials writes plaintext credentials into secrets.Store and
// returns the reference map for persistence. Credential keys that arrive
// empty are skipped — they're interpreted as "caller didn't touch this
// credential," not as "set it to empty."
func (s *PluginServer) stashCredentials(pluginID, connID string, creds map[string]string) (map[string]string, error) {
	refs := map[string]string{}
	for key, plain := range creds {
		if plain == "" {
			continue
		}
		// Skip values that are already secret:// references (used when
		// callers pass through a redacted read — rare, but not an error).
		if secrets.IsReference(plain) {
			refs[key] = plain
			continue
		}
		storeKey := fmt.Sprintf("plugins/%s/%s/%s", sanitizeKey(pluginID), connID, key)
		ref, err := secrets.StoreAsReference(s.secrets, storeKey, plain)
		if err != nil {
			return nil, err
		}
		refs[key] = ref
	}
	return refs, nil
}

// sanitizeKey replaces dots with slashes so the secret path is well-formed
// for keyring backends that use "." as a separator.
func sanitizeKey(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '.' {
			out = append(out, '/')
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

func toConnectionResponse(c *domain.Connection) ConnectionResponse {
	if c == nil {
		return ConnectionResponse{}
	}
	creds := map[string]bool{}
	for k, v := range c.CredentialRefs {
		creds[k] = v != ""
	}
	return ConnectionResponse{
		ID:                    c.ID,
		PluginID:              c.PluginID,
		Name:                  c.Name,
		Config:                c.Config,
		Credentials:           creds,
		Enabled:               c.Enabled,
		WebhookURL:            c.WebhookURL,
		WebhookEnabled:        c.WebhookEnabled,
		WebhookEventAllowlist: c.WebhookEventAllowlist,
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
	}
}

func toConnectionResponses(conns []*domain.Connection) []ConnectionResponse {
	out := make([]ConnectionResponse, 0, len(conns))
	for _, c := range conns {
		out = append(out, toConnectionResponse(c))
	}
	return out
}

// _ imports kept to avoid "imported and not used" warnings when the
// gin-validated Config-field spec eventually grows out of this server.
var _ = json.Marshal
