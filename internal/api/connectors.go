package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/nomi/internal/connectors"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// ConnectorServer handles connector-related endpoints
type ConnectorServer struct {
	registry   *connectors.Registry
	configRepo *db.ConnectorConfigRepository
	secrets    secrets.Store
}

// NewConnectorServer creates a new connector server. The secrets store is
// used to redact bot tokens on read and to stash newly-supplied plaintext
// tokens on write.
func NewConnectorServer(registry *connectors.Registry, database *db.DB, secretStore secrets.Store) *ConnectorServer {
	return &ConnectorServer{
		registry:   registry,
		configRepo: db.NewConnectorConfigRepository(database),
		secrets:    secretStore,
	}
}

// ListConnectors lists all registered connectors with their manifests
func (s *ConnectorServer) ListConnectors(c *gin.Context) {
	list := s.registry.List()
	manifests := make([]connectors.ConnectorManifest, 0, len(list))
	for _, conn := range list {
		manifests = append(manifests, conn.Manifest())
	}
	c.JSON(http.StatusOK, gin.H{"connectors": manifests})
}

// GetConnectorStatus returns the status of a specific connector
func (s *ConnectorServer) GetConnectorStatus(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		respondValidationError(c, "name is required")
		return
	}

	status, err := s.registry.Status(name)
	if err != nil {
		respondNotFound(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, status)
}

// ListConnectorStatuses returns the status of all connectors
func (s *ConnectorServer) ListConnectorStatuses(c *gin.Context) {
	statuses := s.registry.AllStatuses()
	c.JSON(http.StatusOK, gin.H{"statuses": statuses})
}

// ConnectorConfigResponse represents a connector with its config
type ConnectorConfigResponse struct {
	Name         string                        `json:"name"`
	Manifest     connectors.ConnectorManifest  `json:"manifest"`
	Status       connectors.ConnectorStatus    `json:"status"`
	Config       map[string]interface{}        `json:"config"`
	Enabled      bool                          `json:"enabled"`
}

// ListConnectorConfigs returns all connectors with their configurations.
// Secret-bearing fields are replaced with an opaque "<stored>" marker so the
// UI can tell that a value is configured without the bytes ever being sent
// back over the wire. This matters even on loopback because the bearer token
// isn't a confidentiality guarantee against disk-resident logs or audit.
func (s *ConnectorServer) ListConnectorConfigs(c *gin.Context) {
	all := s.registry.List()
	configs := make([]ConnectorConfigResponse, 0, len(all))

	for _, conn := range all {
		status, _ := s.registry.Status(conn.Name())
		if status == nil {
			status = &connectors.ConnectorStatus{Name: conn.Name()}
		}

		record, err := s.configRepo.Get(conn.Name())
		var config map[string]interface{}
		enabled := false
		if err == nil && record != nil {
			enabled = record.Enabled
			if len(record.Config) > 0 {
				_ = json.Unmarshal(record.Config, &config)
			}
		}

		if conn.Name() == "telegram" {
			redactTelegramSecrets(config)
		}

		configs = append(configs, ConnectorConfigResponse{
			Name:     conn.Name(),
			Manifest: conn.Manifest(),
			Status:   *status,
			Config:   config,
			Enabled:  enabled,
		})
	}

	c.JSON(http.StatusOK, gin.H{"connectors": configs})
}

// redactTelegramSecrets replaces every bot_token value in the config map
// with a boolean flag so the UI can render "configured" vs "not configured"
// without ever seeing the raw token or reference URI.
func redactTelegramSecrets(config map[string]interface{}) {
	rawConns, ok := config["connections"].([]interface{})
	if !ok {
		return
	}
	for _, raw := range rawConns {
		conn, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if tok, _ := conn["bot_token"].(string); tok != "" {
			conn["bot_token"] = ""
			conn["bot_token_configured"] = true
		} else {
			conn["bot_token_configured"] = false
		}
	}
}

// UpdateConnectorConfigRequest represents a request to update connector config
type UpdateConnectorConfigRequest struct {
	Config  map[string]interface{} `json:"config"`
	Enabled bool                   `json:"enabled"`
}

// UpdateConnectorConfig updates a connector's configuration and restarts it.
//
// For connectors that carry secrets (currently Telegram's bot_token), any
// plaintext value supplied in the request is stashed in the secrets store
// and replaced in-place with a secret:// reference before the config is
// persisted. This means the SQLite row never sees the raw token, only the
// reference.
func (s *ConnectorServer) UpdateConnectorConfig(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		respondValidationError(c, "name is required")
		return
	}

	var req UpdateConnectorConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondValidationError(c, err.Error())
		return
	}

	if name == "telegram" && s.secrets != nil {
		if err := stashTelegramSecrets(s.secrets, req.Config); err != nil {
			respondInternal(c, "failed to stash secrets", err)
			return
		}
	}

	configJSON, err := json.Marshal(req.Config)
	if err != nil {
		respondValidationError(c, "invalid config")
		return
	}

	if err := s.configRepo.Upsert(name, configJSON, req.Enabled); err != nil {
		respondInternal(c, "failed to upsert connector config", err)
		return
	}

	// Telegram bridge: the old /connectors/telegram/config endpoint is
	// still the UI's only way to add bots until plugin-ui-01 lands. Mirror
	// the write into plugin_connections + assistant_connection_bindings so
	// the TelegramPlugin sees the new connection immediately. Removing
	// this block after plugin-ui-01 ships is a one-line change.
	if name == "telegram" {
		if err := s.mirrorTelegramToPlugin(req.Config, req.Enabled); err != nil {
			// Surface as a warning rather than a hard error — the legacy
			// row persisted, the bridge failure only affects the new
			// plugin's visibility.
			log.Printf("telegram bridge write failed: %v", err)
		}
	}

	// Restart the connector to apply changes dynamically
	if err := s.registry.Restart(c.Request.Context(), name); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"status": "updated",
			"message": "Configuration saved but restart failed: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "updated",
		"message": "Configuration saved and applied.",
	})
}

// mirrorTelegramToPlugin dual-writes the old Telegram config into the new
// plugin-architecture tables. Fields are mapped 1:1:
//
//	connector_configs.config.connections[i] → plugin_connections row
//	  with plugin_id = "com.nomi.telegram" and credential_refs.bot_token
//	  set to the secret:// reference previously stashed by
//	  stashTelegramSecrets.
//	default_assistant_id → assistant_connection_bindings row with
//	  role = "channel" and is_primary = true.
//
// The function is idempotent: each connection is upserted by id, and
// orphaned rows (connection present in plugin_connections but missing
// from the incoming config) are NOT auto-deleted — we only delete
// something when the user explicitly removes it from the UI. This is
// conservative on the "don't surprise the user" axis.
func (s *ConnectorServer) mirrorTelegramToPlugin(config map[string]interface{}, topLevelEnabled bool) error {
	rawConns, _ := config["connections"].([]interface{})

	connRepo := db.NewConnectionRepository(s.configRepo.DB())
	bindRepo := db.NewAssistantBindingRepository(s.configRepo.DB())

	for _, raw := range rawConns {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := c["id"].(string)
		if id == "" {
			continue
		}
		name, _ := c["name"].(string)
		tokenRef, _ := c["bot_token"].(string) // already a secret:// ref after stashTelegramSecrets
		connEnabled, _ := c["enabled"].(bool)
		defaultAsst, _ := c["default_assistant_id"].(string)

		existing, _ := connRepo.GetByID(id)
		if existing == nil {
			_ = connRepo.Create(&domain.Connection{
				ID:             id,
				PluginID:       telegramPluginID,
				Name:           name,
				Config:         map[string]interface{}{},
				CredentialRefs: map[string]string{"bot_token": tokenRef},
				Enabled:        connEnabled && topLevelEnabled,
			})
		} else {
			existing.Name = name
			existing.CredentialRefs["bot_token"] = tokenRef
			existing.Enabled = connEnabled && topLevelEnabled
			_ = connRepo.Update(existing)
		}

		if defaultAsst != "" {
			_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
				AssistantID:  defaultAsst,
				ConnectionID: id,
				Role:         domain.BindingRoleChannel,
				Enabled:      true,
				IsPrimary:    true,
			})
		}
	}
	return nil
}

// telegramPluginID mirrors internal/plugins/telegram.PluginID without
// importing the plugin package (which would create an import cycle
// through the runtime).
const telegramPluginID = "com.nomi.telegram"

// stashTelegramSecrets walks req.Config looking like a TelegramConfig and
// moves any plaintext bot_token values into the secrets store, replacing
// them in the config map with secret:// references. Values that already
// look like references are left alone.
func stashTelegramSecrets(store secrets.Store, config map[string]interface{}) error {
	rawConns, ok := config["connections"].([]interface{})
	if !ok {
		return nil // nothing to migrate
	}
	for _, raw := range rawConns {
		conn, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		token, ok := conn["bot_token"].(string)
		if !ok || token == "" || secrets.IsReference(token) {
			continue
		}
		id, _ := conn["id"].(string)
		if id == "" {
			// Connections added via the UI should always have an ID, but
			// guard against user-supplied configs missing one.
			return fmt.Errorf("connection missing id; cannot safely stash its bot_token")
		}
		key := fmt.Sprintf("connector/telegram/%s/bot_token", id)
		ref, err := secrets.StoreAsReference(store, key, token)
		if err != nil {
			return err
		}
		conn["bot_token"] = ref
		log.Printf("secrets: stashed telegram[%s] bot_token → %s", id, ref)
	}
	return nil
}

// RestartConnectorRequest represents a request to restart a connector
type RestartConnectorRequest struct {
	Enabled bool `json:"enabled"`
}

// RestartConnector restarts a connector to apply new configuration
func (s *ConnectorServer) RestartConnector(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		respondValidationError(c, "name is required")
		return
	}

	if err := s.registry.Restart(c.Request.Context(), name); err != nil {
		respondInternal(c, "failed to restart connector", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "restarted"})
}
