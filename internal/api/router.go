package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/felixgeelhaar/nomi/internal/buildinfo"
	"github.com/felixgeelhaar/nomi/internal/connectors"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/metrics"
	"github.com/felixgeelhaar/nomi/internal/memstore"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/plugins"
	"github.com/felixgeelhaar/nomi/internal/plugins/hub"
	"github.com/felixgeelhaar/nomi/internal/plugins/signing"
	"github.com/felixgeelhaar/nomi/internal/plugins/store"
	"github.com/felixgeelhaar/nomi/internal/plugins/wasmhost"
	"github.com/felixgeelhaar/nomi/internal/runtime"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
	"github.com/felixgeelhaar/nomi/internal/tunnel"
	"github.com/felixgeelhaar/nomi/internal/webhooks"
)

// RouterConfig bundles the dependencies and security knobs the router needs.
type RouterConfig struct {
	Runtime    *runtime.Runtime
	DB         *db.DB
	EventBus   *events.EventBus
	Approvals  *permissions.Manager
	Memory     *memory.Manager
	MemoryClient memstore.Client // optional; required for /memory/export + /memory/import endpoints (ADR 0004 §8)
	Tools      *tools.Registry
	Connectors  *connectors.Registry
	Plugins *plugins.Registry // source of truth for plugin-architecture endpoints
	Secrets secrets.Store
	AuthToken   string // required; requests must carry Authorization: Bearer <AuthToken>

	// AuthTokenStore optionally lets the auth middleware pick up rotated
	// tokens at runtime. When non-nil, the middleware reads from the
	// store and AuthToken is used only as a fallback for tests.
	AuthTokenStore *TokenStore

	// Tunnel exposes the local API to the internet for inbound webhooks.
	// nil disables the webhook surface.
	Tunnel tunnel.Adapter

	// Marketplace install/uninstall dependencies (lifecycle-07). All
	// three must be present for POST /plugins/install + DELETE
	// /plugins/:id to be enabled; absent collaborators leave the
	// surface returning 503.
	PluginStore    *store.Store
	PluginVerifier *signing.Verifier
	WASMLoader     *wasmhost.Loader
	// CatalogProvider supplies the parsed marketplace catalog for
	// GET /plugins/marketplace (lifecycle-09). nil disables the
	// endpoint without affecting install/uninstall.
	CatalogProvider func(ctx context.Context) (*hub.Catalog, error)
	// PluginUpdater backs POST /plugins/:id/update (lifecycle-10).
	// nil disables the endpoint.
	PluginUpdater func(ctx context.Context, pluginID string) (*domain.PluginState, error)

	// RemoteTemplates enables the remote assistant templates marketplace.
	// nil disables the endpoint.
	RemoteTemplates *db.RemoteTemplateRepository
}

// NewRouter assembles the HTTP routes and wraps them in CORS + auth middleware.
// The returned engine uses gin.New() (not Default) so access logging is controlled
// explicitly and request bodies are never logged.
func NewRouter(cfg RouterConfig) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestIDMiddleware())
	r.Use(accessLogMiddleware())
	r.Use(CORSMiddleware())
	if cfg.AuthTokenStore != nil {
		r.Use(RequireAuthToken(cfg.AuthTokenStore))
	} else {
		r.Use(RequireAuthToken(cfg.AuthToken))
	}

	// Health check (public — allows the UI to probe reachability before the token loads)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Build info (public — the auto-updater and the desktop About panel
	// need to know what version is running before any user interaction
	// has loaded the bearer token).
	r.GET("/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, buildinfo.Current())
	})

	// Prometheus scrape endpoint (public — see auth.go publicPaths).
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{})))

	// Config export / import. Snapshot the user-configured surface as
	// YAML, restore it on another machine. Secrets are exported as
	// references only; plaintext stays in the secrets store.
	configServer := NewConfigServer(cfg.DB, cfg.Secrets)
	r.GET("/config/export", configServer.Export)
	r.POST("/config/import", configServer.Import)

	// Auth-token rotation. Authenticated under the CURRENT token; the
	// response carries the new token once. Disabled when the router was
	// built with a static token (tests) since rotation needs the store.
	if cfg.AuthTokenStore != nil {
		store := cfg.AuthTokenStore
		r.POST("/auth/rotate", func(c *gin.Context) {
			next, err := store.Rotate()
			if err != nil {
				respondInternal(c, "failed to rotate auth token", err)
				return
			}
			c.JSON(http.StatusOK, gin.H{"token": next})
		})
	}

	// Run endpoints
	runServer := NewServer(cfg.Runtime)
	runs := r.Group("/runs")
	{
		runs.POST("", runServer.CreateRun)
		runs.GET("", runServer.ListRuns)
		runs.GET("/:id", runServer.GetRun)
		runs.GET("/:id/approvals", runServer.GetRunApprovals)
		runs.POST("/:id/approve", runServer.ApproveRun)
		runs.POST("/:id/plan/approve", runServer.ApprovePlan)
		runs.POST("/:id/plan/edit", runServer.EditPlan)
		runs.POST("/:id/fork", runServer.ForkRun)
		runs.POST("/:id/retry", runServer.RetryRun)
		runs.POST("/:id/replan", runServer.ReplanRun)
		runs.POST("/:id/pause", runServer.PauseRun)
		runs.POST("/:id/resume", runServer.ResumeRun)
		runs.POST("/:id/cancel", runServer.CancelRun)
		runs.DELETE("/:id", runServer.DeleteRun)
	}

	// Runtime introspection — surface the registered executor backends so
	// the desktop UI can populate the assistant builder dropdown without
	// hardcoding the list. Returns names sorted alphabetically.
	if cfg.Runtime != nil {
		r.GET("/runtime/executor-backends", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"backends": cfg.Runtime.ExecutorBackends(),
			})
		})
	}

	// Assistant endpoints
	assistantServer := NewAssistantServer(cfg.DB, cfg.EventBus)
	assistants := r.Group("/assistants")
	{
		assistants.POST("", assistantServer.CreateAssistant)
		assistants.GET("/templates", assistantServer.ListTemplates)
		assistants.GET("", assistantServer.ListAssistants)
		assistants.GET("/:id", assistantServer.GetAssistant)
		assistants.PUT("/:id", assistantServer.UpdateAssistant)
		assistants.POST("/:id/apply-safety-profile", assistantServer.ApplySafetyProfileToAssistant)
		assistants.DELETE("/:id", assistantServer.DeleteAssistant)
	}

	// Approval endpoints
	approvalServer := NewApprovalServer(cfg.Approvals, cfg.DB)
	approvals := r.Group("/approvals")
	{
		approvals.GET("", approvalServer.ListApprovals)
		approvals.GET("/:id", approvalServer.GetApproval)
		approvals.POST("/:id/resolve", approvalServer.ResolveApproval)
	}

	// Event endpoints
	eventServer := NewEventServer(cfg.EventBus)
	eventsGroup := r.Group("/events")
	{
		eventsGroup.GET("", eventServer.ListEvents)
		eventsGroup.GET("/stream", eventServer.StreamEvents)
	}

	// Memory endpoints
	memoryServer := NewMemoryServer(cfg.Memory, cfg.MemoryClient)
	memories := r.Group("/memory")
	{
		memories.POST("", memoryServer.CreateMemory)
		memories.GET("", memoryServer.ListMemory)
		memories.GET("/export", memoryServer.ExportMemory)
		memories.POST("/import", memoryServer.ImportMemory)
		memories.GET("/:id", memoryServer.GetMemory)
		memories.DELETE("/:id", memoryServer.DeleteMemory)
	}

	// Tools endpoints
	toolsServer := NewToolsServer(cfg.Tools)
	toolsGroup := r.Group("/tools")
	{
		toolsGroup.POST("/filesystem.context/preview", toolsServer.PreviewFolderContext)
	}

	// Connector endpoints
	connectorServer := NewConnectorServer(cfg.Connectors, cfg.DB, cfg.Secrets)
	connectorsGroup := r.Group("/connectors")
	{
		connectorsGroup.GET("", connectorServer.ListConnectors)
		connectorsGroup.GET("/statuses", connectorServer.ListConnectorStatuses)
		connectorsGroup.GET("/:name/status", connectorServer.GetConnectorStatus)
		connectorsGroup.GET("/configs", connectorServer.ListConnectorConfigs)
		connectorsGroup.PUT("/:name/config", connectorServer.UpdateConnectorConfig)
	}

	// Plugin endpoints (ADR 0001). The new architecture's REST surface
	// for listing plugins, managing connections, and configuring
	// assistant bindings. The legacy /connectors/... surface survives
	// for the old UI until plugin-ui-01 replaces it.
	if cfg.Plugins != nil {
		pluginServer := NewPluginServer(cfg.Plugins, db.NewConnectionRepository(cfg.DB), db.NewAssistantBindingRepository(cfg.DB), db.NewPluginStateRepository(cfg.DB), cfg.Secrets)
		// Marketplace install pathway is opt-in: all three deps must be
		// present (store, verifier, loader). When wired, install +
		// uninstall endpoints are enabled; otherwise they 503 with a
		// clear error so the UI can show "marketplace disabled."
		if cfg.PluginStore != nil && cfg.PluginVerifier != nil && cfg.WASMLoader != nil {
			pluginServer.AttachInstall(InstallDependencies{
				Store:           cfg.PluginStore,
				Verifier:        cfg.PluginVerifier,
				Loader:          cfg.WASMLoader,
				CatalogProvider: cfg.CatalogProvider,
				Updater:         cfg.PluginUpdater,
			})
		}
		pluginGroup := r.Group("/plugins")
		{
			pluginGroup.GET("", pluginServer.ListPlugins)
			pluginGroup.GET("/marketplace", pluginServer.MarketplaceCatalog)
			pluginGroup.POST("/install", pluginServer.InstallPlugin)
			pluginGroup.GET("/:id", pluginServer.GetPlugin)
			pluginGroup.POST("/:id/update", pluginServer.UpdatePlugin)
			pluginGroup.DELETE("/:id", pluginServer.UninstallPlugin)
			pluginGroup.GET("/:id/state", pluginServer.GetPluginState)
			pluginGroup.PATCH("/:id/state", pluginServer.PatchPluginState)
			pluginGroup.POST("/:id/connections", pluginServer.CreateConnection)
			pluginGroup.PATCH("/:id/connections/:conn_id", pluginServer.UpdateConnection)
			pluginGroup.DELETE("/:id/connections/:conn_id", pluginServer.DeleteConnection)
		}

		// Identity allowlist endpoints (ADR 0001 §9). Nested under a
		// plugin + connection because allowlist entries are per-connection.
		identityServer := NewIdentityServer(db.NewChannelIdentityRepository(cfg.DB))
		identityGroup := pluginGroup.Group("/:id/connections/:conn_id/identities")
		{
			identityGroup.GET("", identityServer.ListIdentities)
			identityGroup.POST("", identityServer.CreateIdentity)
			identityGroup.PATCH("/:ident_id", identityServer.UpdateIdentity)
			identityGroup.DELETE("/:ident_id", identityServer.DeleteIdentity)
		}

		// Email trigger rules (task-email-plugin). Nested under
		// /plugins/:id/connections/:conn_id/trigger-rules so the UI
		// can manage rules per-connection. Guarded to email plugin only.
		emailTriggerServer := NewEmailTriggerServer(db.NewEmailTriggerRepository(cfg.DB))
		emailTriggerGroup := pluginGroup.Group("/:id/connections/:conn_id/trigger-rules")
		{
			emailTriggerGroup.GET("", emailTriggerServer.ListEmailTriggerRules)
			emailTriggerGroup.POST("", emailTriggerServer.CreateEmailTriggerRule)
			emailTriggerGroup.PUT("/:name", emailTriggerServer.UpdateEmailTriggerRule)
			emailTriggerGroup.DELETE("/:name", emailTriggerServer.DeleteEmailTriggerRule)
		}

		assistantBindings := r.Group("/assistants")
		{
			assistantBindings.GET("/:id/bindings", pluginServer.ListAssistantBindings)
			assistantBindings.POST("/:id/bindings", pluginServer.UpsertAssistantBinding)
			assistantBindings.DELETE("/:id/bindings/:conn_id/:role", pluginServer.DeleteAssistantBinding)
		}
	}

	// Conversation endpoints (ADR 0001 §8). Read-mostly — conversations
	// are created by channel plugins, not by REST clients.
	conversationServer := NewConversationServer(
		db.NewConversationRepository(cfg.DB),
		db.NewRunRepository(cfg.DB),
	)
	conversations := r.Group("/conversations")
	{
		conversations.GET("", conversationServer.ListConversations)
		conversations.GET("/:id", conversationServer.GetConversation)
		conversations.DELETE("/:id", conversationServer.DeleteConversation)
	}


	// Provider profile endpoints
	providerServer := NewProviderServer(cfg.DB, cfg.Secrets)
	providers := r.Group("/provider-profiles")
	{
		providers.POST("", providerServer.CreateProviderProfile)
		providers.POST("/probe", providerServer.ProbeProvider)
		providers.GET("", providerServer.ListProviderProfiles)
		providers.GET("/:id", providerServer.GetProviderProfile)
		providers.PUT("/:id", providerServer.UpdateProviderProfile)
		providers.DELETE("/:id", providerServer.DeleteProviderProfile)
	}

	// Settings endpoints
	settings := r.Group("/settings")
	{
		settings.GET("/llm-default", providerServer.GetLLMDefaultSettings)
		settings.PUT("/llm-default", providerServer.SetLLMDefaultSettings)
		settings.GET("/onboarding-complete", providerServer.GetOnboardingComplete)
		settings.PUT("/onboarding-complete", providerServer.SetOnboardingComplete)
		settings.GET("/safety-profile", providerServer.GetSafetyProfile)
		settings.PUT("/safety-profile", providerServer.SetSafetyProfile)
	}

	auditServer := NewAuditServer(cfg.DB, cfg.AuthToken)
	audit := r.Group("/audit")
	{
		audit.GET("/export", auditServer.Export)
		audit.GET("/verify", auditServer.Verify)
		audit.POST("/prune", auditServer.Prune)
	}

	// Webhook receiver surface (public — external services have no bearer token).
	// Signature verification is the security boundary.
	if cfg.Plugins != nil && cfg.Secrets != nil && cfg.Runtime != nil {
		whRouter := webhooks.NewRouter(webhooks.RouterConfig{
			PluginRegistry: cfg.Plugins,
			ConnectionRepo: db.NewConnectionRepository(cfg.DB),
			BindingRepo:    db.NewAssistantBindingRepository(cfg.DB),
			Secrets:        cfg.Secrets,
			Runtime:        cfg.Runtime,
			EventBus:       cfg.EventBus,
		})
		whRouter.Mount(r)
	}

	// Webhook management endpoints (authenticated). Namespaced under
	// /webhook-admin/ so per-connection wildcards don't collide with the
	// public /webhooks/:plugin_id/:connection_id inbound receiver mounted
	// by the webhooks package.
	webhookServer := NewWebhookServer(cfg.DB, cfg.Secrets, cfg.Tunnel)
	webhookAdminGroup := r.Group("/webhook-admin")
	{
		webhookAdminGroup.GET("/tunnel", webhookServer.GetTunnelStatus)
		webhookAdminGroup.POST("/:connection_id/rotate-secret", webhookServer.RotateSecret)
		webhookAdminGroup.PUT("/:connection_id/allowlist", webhookServer.UpdateAllowlist)
	}

	// Remote assistant templates marketplace
	if cfg.RemoteTemplates != nil {
		remoteTemplateServer := NewRemoteTemplateServer(cfg.RemoteTemplates)
		remoteTemplates := r.Group("/remote-templates")
		{
			remoteTemplates.GET("", remoteTemplateServer.ListRemoteTemplates)
			remoteTemplates.POST("/install", remoteTemplateServer.InstallRemoteTemplate)
		}
	}

	return r
}
