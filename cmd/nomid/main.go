package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/mnemos/embedded"
	"github.com/felixgeelhaar/nomi/internal/api"
	"github.com/felixgeelhaar/nomi/internal/buildinfo"
	"github.com/felixgeelhaar/nomi/internal/connectors"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/integrations/google"
	"github.com/felixgeelhaar/nomi/internal/llm"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/plugins"
	browserplugin "github.com/felixgeelhaar/nomi/internal/plugins/browser"
	calendarplugin "github.com/felixgeelhaar/nomi/internal/plugins/calendar"
	discordplugin "github.com/felixgeelhaar/nomi/internal/plugins/discord"
	emailplugin "github.com/felixgeelhaar/nomi/internal/plugins/email"
	githubplugin "github.com/felixgeelhaar/nomi/internal/plugins/github"
	gmailplugin "github.com/felixgeelhaar/nomi/internal/plugins/gmail"
	mediaplugin "github.com/felixgeelhaar/nomi/internal/plugins/media"
	obsidianplugin "github.com/felixgeelhaar/nomi/internal/plugins/obsidian"
	slackplugin "github.com/felixgeelhaar/nomi/internal/plugins/slack"
	telegramplugin "github.com/felixgeelhaar/nomi/internal/plugins/telegram"
	"github.com/felixgeelhaar/nomi/internal/plugins/devloader"
	"github.com/felixgeelhaar/nomi/internal/plugins/hub"
	"github.com/felixgeelhaar/nomi/internal/plugins/signing"
	"github.com/felixgeelhaar/nomi/internal/plugins/store"
	"github.com/felixgeelhaar/nomi/internal/plugins/update"
	"github.com/felixgeelhaar/nomi/internal/plugins/wasmhost"
	"github.com/felixgeelhaar/nomi/internal/plugins/wasmplugin"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"sync"
	"github.com/felixgeelhaar/nomi/internal/runtime"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/seed"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
	"github.com/felixgeelhaar/nomi/internal/tunnel"
)

func main() {
	// Handle --version before any other startup work so packagers, CI,
	// and the auto-updater can ask "what version is this binary?"
	// without booting the daemon.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "-v", "version":
			fmt.Printf("nomid %s\ncommit:     %s\nbuild date: %s\n",
				buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)
			return
		}
	}

	// Default gin to release mode so the daemon doesn't print a per-route
	// debug banner + per-request logger output that no production user
	// asked for. NOMI_DEV=1 keeps the noisy mode for local hacking.
	if os.Getenv("NOMI_DEV") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	log.Println("Nomi Runtime (nomid) starting...")
	log.Printf("Version: %s (commit %s, built %s)",
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)

	// Database setup
	config := db.DefaultConfig()
	database, err := db.New(config)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer database.Close()

	// Run migrations (embedded in binary)
	if err := database.Migrate(); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}
	log.Println("Database migrations completed")

	// Bearer token for HTTP API (persisted next to the database with 0600 perms)
	dataDir := filepath.Dir(config.Path)
	authToken, tokenPath, err := api.LoadOrGenerateAuthToken(dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize auth token: %v", err)
	}
	log.Printf("API auth token at %s", tokenPath)

	// Secret store: OS keyring if available, else encrypted file vault in
	// the data dir. Must be set up before any connector or provider code
	// path touches a token.
	secretStore, backend, err := secrets.NewStore(dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize secret store: %v", err)
	}
	log.Printf("Secret store backend: %s", backend)

	// One-time migration of any plaintext secrets still present in SQLite.
	// Idempotent: values already stored as references pass through.
	if err := secrets.MigrateRepositories(secretStore, database); err != nil {
		log.Printf("Secret migration encountered errors: %v", err)
	}

	// Load application settings
	settingsRepo := db.NewAppSettingsRepository(database)

	// Headless / docker / k8s bootstrap. Reads a YAML manifest the
	// daemon's first boot, applies it idempotently. Path resolution
	// order: $NOMI_SEED env var → $NOMI_DATA_DIR/seed.yaml. Idempotent;
	// editing the file and restarting picks up the diff. A malformed
	// seed logs but does not abort startup — a previously-working
	// install shouldn't fall over because of a typo in seed.yaml.
	seedPath := os.Getenv("NOMI_SEED")
	if seedPath == "" {
		seedPath = filepath.Join(dataDir, "seed.yaml")
	}
	if err := seed.Apply(seedPath, seed.Deps{
		DB:         database,
		Providers:  db.NewProviderProfileRepository(database),
		Assistants: db.NewAssistantRepository(database),
		Settings:   settingsRepo,
		Globals:    db.NewGlobalSettingsRepository(database),
		Secrets:    secretStore,
	}); err != nil {
		log.Printf("seed: %v (continuing without seeded state)", err)
	}

	// Event system
	eventStore := db.NewEventRepository(database)
	eventBus := events.NewEventBus(eventStore)

	// Permission system
	permEngine := permissions.NewEngine()
	approvalStore := db.NewApprovalRepository(database)
	approvalMgr := permissions.NewApprovalManager(approvalStore, eventBus)

	// Tool system
	toolRegistry := tools.NewRegistry()
	if err := tools.RegisterCoreTools(toolRegistry); err != nil {
		log.Fatalf("Failed to register core tools: %v", err)
	}

	// LLM resolver + llm.chat tool. The resolver is consulted at
	// tool-invocation time rather than at boot, so the daemon doesn't need
	// a provider configured to start — it only fails when someone actually
	// tries to run a step that needs an LLM.
	llmResolver := llm.NewResolver(
		db.NewProviderProfileRepository(database),
		db.NewGlobalSettingsRepository(database),
		secretStore,
	)
	if err := toolRegistry.Register(tools.NewLLMChatTool(llmResolver)); err != nil {
		log.Fatalf("Failed to register llm.chat tool: %v", err)
	}

	toolExecutor := tools.NewExecutor(toolRegistry)

	// Memory system. Runtime + /memory/export + /memory/import go through
	// the standalone Mnemos embedded backend at <dataDir>/mnemos.db
	// (ADR 0004 step 2). REST CRUD endpoints (POST/GET/DELETE /memory)
	// still write to nomi.db's memory table via *memory.Manager —
	// migration of those handlers to mnemos.Client is a follow-up.
	memoryRepo := db.NewMemoryRepository(database)
	memManager := memory.NewManager(memoryRepo)

	mnemosDBPath := filepath.Join(dataDir, "mnemos.db")
	memClient, err := embedded.Open(mnemosDBPath)
	if err != nil {
		slog.Error("open mnemos store", "path", mnemosDBPath, "error", err)
		os.Exit(1)
	}
	defer memClient.Close()
	memClient.WithEmitter(memory.NewBusEmitter(eventBus))

	// First-boot migration: if nomi.db has rows in its legacy memory
	// table and mnemos.db is empty, copy them over once. Records a
	// completion marker in app_settings so subsequent boots skip the
	// step. Best-effort: failure logs but does not abort startup.
	if err := memory.MigrateLegacyMemory(context.Background(), database, memClient, memManager); err != nil {
		slog.Warn("legacy memory migration", "error", err)
	}

	// Runtime
	rt := runtime.NewRuntime(database, eventBus, permEngine, approvalMgr, toolExecutor, memClient, runtime.DefaultConfig())
	rt.SetLLMResolver(llmResolver)

	// Plugin system (ADR 0001). plugins.Registry is now the source of truth
	// for channels/tools/triggers/context_sources. connectors.Registry
	// remains only to keep the old /connectors REST surface operational
	// for the existing UI until the Plugins tab lands; no plugins register
	// into it in production anymore.
	pluginRegistry := plugins.NewRegistry()
	connRegistry := connectors.NewRegistry()
	connectionRepo := db.NewConnectionRepository(database)
	bindingRepo := db.NewAssistantBindingRepository(database)
	conversationRepo := db.NewConversationRepository(database)
	identityRepo := db.NewChannelIdentityRepository(database)
	emailTriggerRepo := db.NewEmailTriggerRepository(database)

	// Telegram plugin — migrated from the old connector shape. Reads from
	// plugin_connections + assistant_connection_bindings. The data
	// migration 000011 backfills existing Telegram connector_configs rows
	// into the new tables, so running users see no behavior change.
	// Conversation repo enables multi-turn threading (ADR 0001 §8);
	// identity repo enables per-(connection,sender) allowlist enforcement
	// (ADR 0001 §9). Both are optional at construction.
	telegramPlugin := telegramplugin.NewPlugin(
		rt, connectionRepo, bindingRepo, conversationRepo, identityRepo,
		db.NewRunRepository(database), approvalMgr, eventBus, secretStore,
	)
	if err := pluginRegistry.Register(telegramPlugin); err != nil {
		log.Fatalf("Failed to register Telegram plugin: %v", err)
	}

	// Email plugin — generic IMAP/SMTP channel. Provider-specific flavors
	// (Gmail/Outlook OAuth) land as their own plugins on top of this
	// transport layer.
	emailPlugin := emailplugin.NewPlugin(rt, connectionRepo, bindingRepo, conversationRepo, identityRepo, emailTriggerRepo, secretStore, eventBus)
	if err := pluginRegistry.Register(emailPlugin); err != nil {
		log.Fatalf("Failed to register Email plugin: %v", err)
	}

	// Slack plugin — Socket Mode (no webhook hosting required). User installs
	// a Slack app in their workspace, pastes the bot token + app-level
	// token, and the bot can DM + be @mentioned.
	slackPlugin := slackplugin.NewPlugin(
		rt, connectionRepo, bindingRepo, conversationRepo, identityRepo,
		db.NewRunRepository(database), approvalMgr, eventBus, secretStore,
	)
	if err := pluginRegistry.Register(slackPlugin); err != nil {
		log.Fatalf("Failed to register Slack plugin: %v", err)
	}

	// Discord plugin — Gateway WebSocket. User creates a Discord
	// application, pastes the bot token, invites the bot to servers they
	// want it in.
	discordPlugin := discordplugin.NewPlugin(rt, connectionRepo, bindingRepo, conversationRepo, identityRepo, secretStore, eventBus)
	if err := pluginRegistry.Register(discordPlugin); err != nil {
		log.Fatalf("Failed to register Discord plugin: %v", err)
	}

	// Google OAuth manager — shared across any Google-backed plugin
	// (Calendar today; Gmail when that plugin lands). Constructed before
	// the Calendar registration so the plugin can wire it in.
	googleOAuth := google.NewOAuthManager(secretStore)

	// Calendar plugin — tool-only. Uses the shared Google OAuth manager.
	// Outlook support will layer on behind the same Provider interface.
	calendarPlugin := calendarplugin.NewPlugin(connectionRepo, bindingRepo, googleOAuth, secretStore)
	if err := pluginRegistry.Register(calendarPlugin); err != nil {
		log.Fatalf("Failed to register Calendar plugin: %v", err)
	}

	// Gmail plugin — tool-only. Same Google OAuth manager as Calendar
	// so a single Google account works across both. SMTP/IMAP email
	// stays the canonical generic email channel; Gmail plugin is for
	// the API-only operations (HTML drafts, label management, thread
	// search) that don't fit the SMTP shape.
	gmailPlugin := gmailplugin.NewPlugin(connectionRepo, bindingRepo, googleOAuth, secretStore)
	if err := pluginRegistry.Register(gmailPlugin); err != nil {
		log.Fatalf("Failed to register Gmail plugin: %v", err)
	}

	// GitHub plugin — tool-only. Per-Connection App installation. The
	// scaffold registers with an empty tool surface; subsequent tasks
	// (github-03..06) wire issues / pulls / repos / polling. Until the
	// user adds a Connection with App credentials, the plugin is inert.
	githubPlugin := githubplugin.NewPlugin(connectionRepo, bindingRepo, secretStore)
	if err := pluginRegistry.Register(githubPlugin); err != nil {
		log.Fatalf("Failed to register GitHub plugin: %v", err)
	}

	// Obsidian Vault plugin — tool-only, filesystem-only. One Connection
	// = one user-selected vault folder. The plugin holds no credentials
	// and has no network capability; tools only ever touch files inside
	// the chosen vault path (sandboxed via tools.ResolveWithinRoot). Stays
	// inert until the user adds a Connection in the Connections tab.
	obsidianPlugin := obsidianplugin.NewPlugin(connectionRepo, bindingRepo)
	if err := pluginRegistry.Register(obsidianPlugin); err != nil {
		log.Fatalf("Failed to register Obsidian plugin: %v", err)
	}

	// Browser plugin — system tier. Spawns Scout
	// (felixgeelhaar/scout) as a subprocess per Connection and
	// proxies tool calls over stdio MCP. Each connection's Scout has
	// its own browser profile so assistants don't share auth state.
	// Requires `scout` on PATH (brew install scout); plugin reports
	// status via the connection-level error if the binary is absent.
	browserPlugin := browserplugin.NewPlugin(connectionRepo, bindingRepo, secretStore)
	if err := pluginRegistry.Register(browserPlugin); err != nil {
		log.Fatalf("Failed to register Browser plugin: %v", err)
	}

	// Media plugin — local-first TTS / STT / vision tools. Backends
	// (Piper, whisper.cpp, …) are wired in main once their detection
	// logic ships in media-08/09; until then the manifest registers
	// but tool calls return a "no backend configured" error.
	mediaPlugin := mediaplugin.NewPlugin()
	// Probe for Piper TTS. NewPiperBackend returns (nil, nil) when the
	// binary isn't installed or no voice models exist — the plugin
	// stays registered, media.speak just surfaces "no backend
	// configured" until the user installs Piper. Hard errors during
	// probe (rare; user-home-dir failure) are surfaced loud.
	if piper, err := mediaplugin.NewPiperBackend("", ""); err != nil {
		log.Printf("piper probe failed: %v", err)
	} else if piper != nil {
		mediaPlugin.SetTTSBackend(piper)
		log.Println("media: Piper TTS backend bound")
	}
	if whisper, err := mediaplugin.NewWhisperBackend("", ""); err != nil {
		log.Printf("whisper probe failed: %v", err)
	} else if whisper != nil {
		mediaPlugin.SetSTTBackend(whisper)
		log.Println("media: whisper.cpp STT backend bound")
	}
	if err := pluginRegistry.Register(mediaPlugin); err != nil {
		log.Fatalf("Failed to register Media plugin: %v", err)
	}

	// Runtime's manifest-intersection lookup now resolves via the plugin
	// registry's CapabilitiesForSource, which keys on channel kind rather
	// than plugin id so run.Source strings ("telegram", "email", …)
	// continue to resolve exactly as before.
	rt.SetConnectorManifestLookup(pluginRegistry.CapabilitiesForSource)

	// Project plugin-contributed tools into the shared tools.Registry so
	// the runtime's executor can dispatch them alongside system tools
	// (filesystem.read, command.exec, llm.chat). Without this, tools like
	// email.send / slack.post_message / discord.post_message wouldn't
	// resolve at plan-execution time even though they're declared on
	// their plugin manifests.
	if err := pluginRegistry.RegisterToolsInto(toolRegistry); err != nil {
		log.Fatalf("Failed to register plugin tools: %v", err)
	}

	// Seed plugin_state for every registered plugin (ADR 0002 §1).
	// Idempotent — preserves the user's enabled/disabled choice across
	// restarts. New plugins added in a future release seed at first
	// boot of the new binary.
	pluginStateRepo := db.NewPluginStateRepository(database)
	for _, p := range pluginRegistry.List() {
		manifest := p.Manifest()
		if err := pluginStateRepo.EnsureSystemPlugin(manifest.ID, manifest.Version); err != nil {
			log.Printf("Failed to seed plugin_state for %s: %v", manifest.ID, err)
		}
	}

	// StartAll honors plugin_state.enabled — a disabled plugin's Start
	// is skipped so its background workers (poll loops, websockets,
	// catalog watchers) never fire until the user re-enables it.
	enabledLookup := func(pluginID string) bool {
		ok, err := pluginStateRepo.IsEnabled(pluginID)
		if err != nil {
			log.Printf("plugin_state lookup for %s: %v (defaulting to enabled)", pluginID, err)
			return true
		}
		return ok
	}
	if err := pluginRegistry.StartAll(context.Background(), enabledLookup); err != nil {
		log.Printf("Failed to start some plugins: %v", err)
	}

	// Re-attach goroutines to any runs left mid-flight by the previous
	// process. Without this, a daemon restart strands any planning /
	// executing / paused run forever.
	if err := rt.ResumeOrphanedRuns(); err != nil {
		log.Printf("Failed to resume orphaned runs: %v", err)
	}

	// Marketplace install dependencies (lifecycle-07). Three pieces:
	//
	//   - On-disk store at <dataDir>/plugins/<id>/ for installed bundles.
	//   - Ed25519 verifier seeded from NOMI_MARKETPLACE_ROOT_KEY (base64
	//     pubkey). Absent → marketplace install is disabled (handlers
	//     return 503); the daemon still boots and bundled plugins work.
	//   - Wasmhost loader shared across plugin instantiations.
	//
	// Future lifecycle-08 (dev plugin loader) reuses store + loader
	// without the verifier.
	pluginStoreRoot := filepath.Join(filepath.Dir(config.Path), "plugins")
	pluginStore, err := store.New(pluginStoreRoot)
	if err != nil {
		log.Fatalf("Failed to init plugin store at %s: %v", pluginStoreRoot, err)
	}
	wasmLoader := wasmhost.NewLoader(context.Background())

	var (
		pluginVerifier  *signing.Verifier
		catalogProvider func(ctx context.Context) (*hub.Catalog, error)
	)
	if rootKeyB64 := os.Getenv("NOMI_MARKETPLACE_ROOT_KEY"); rootKeyB64 != "" {
		raw, decErr := base64.StdEncoding.DecodeString(rootKeyB64)
		if decErr != nil {
			log.Printf("WARN: NOMI_MARKETPLACE_ROOT_KEY not valid base64 (%v) — marketplace install disabled", decErr)
		} else if v, vErr := signing.NewVerifier(ed25519.PublicKey(raw), nil); vErr != nil {
			log.Printf("WARN: marketplace root key rejected (%v) — marketplace install disabled", vErr)
		} else {
			pluginVerifier = v
			log.Printf("Marketplace install enabled (store at %s)", pluginStoreRoot)
			// Catalog provider — bound to the same root pubkey as the
			// install verifier. Fetch-on-demand with a 1h in-process
			// cache so the marketplace endpoint isn't a per-request
			// HTTP hop. Lifecycle-10 swaps this for a daily polling
			// loop that persists the catalog to disk.
			catalogURL := settingsRepo.GetOrDefault("marketplace_catalog_url", "https://hub.nomi.ai/index.json")
			hubClient, hubErr := hub.NewClient(ed25519.PublicKey(raw), nil)
			if hubErr != nil {
				log.Printf("WARN: hub client init: %v", hubErr)
			} else {
				var (
					mu       sync.Mutex
					cached   *hub.Catalog
					cachedAt time.Time
				)
				const ttl = 1 * time.Hour
				catalogProvider = func(ctx context.Context) (*hub.Catalog, error) {
					mu.Lock()
					defer mu.Unlock()
					if cached != nil && time.Since(cachedAt) < ttl {
						return cached, nil
					}
					cat, err := hubClient.Fetch(ctx, catalogURL)
					if err != nil {
						return nil, err
					}
					cached = cat
					cachedAt = time.Now()
					return cat, nil
				}
			}
		}
	} else {
		log.Printf("Marketplace install disabled (set NOMI_MARKETPLACE_ROOT_KEY to enable). Bundled plugins unaffected.")
	}

	// Dev plugin loader (lifecycle-08). Off by default. When the
	// dev_plugins_enabled setting is true the daemon scans
	// <dataDir>/plugins-dev/ for unsigned .nomi-plugin bundles and
	// registers each successful load with distribution=dev so the UI
	// shows the red "unsigned" banner. Flipping the setting back to
	// false does NOT unload — restart is required (matching ADR 0002
	// §2: dev tier is explicit "I'm iterating" mode, not a soft
	// runtime toggle).
	if settingsRepo.GetOrDefault("dev_plugins_enabled", "false") == "true" {
		devDir := filepath.Join(filepath.Dir(config.Path), "plugins-dev")
		stateRepo := db.NewPluginStateRepository(database)
		devRes, devErr := devloader.Load(context.Background(), devDir, wasmLoader)
		if devErr != nil {
			log.Printf("Dev plugin loader: %v", devErr)
		} else {
			for _, e := range devRes.Errors {
				log.Printf("WARN: dev plugin skipped: %v", e)
			}
			for _, p := range devRes.Plugins {
				if err := pluginRegistry.Register(p.Plugin); err != nil {
					log.Printf("WARN: dev plugin %s register failed: %v", p.Manifest.ID, err)
					continue
				}
				_ = stateRepo.Upsert(&domain.PluginState{
					PluginID:     p.Manifest.ID,
					Distribution: domain.PluginDistributionDev,
					Installed:    true,
					Enabled:      true,
					Version:      p.Manifest.Version,
					SourceURL:    p.Path,
					InstalledAt:  time.Now().UTC(),
				})
				log.Printf("Loaded dev plugin %s from %s", p.Manifest.ID, p.Path)
			}
		}
	}

	// Marketplace plugin reload: walk the on-disk store, parse each
	// installed bundle, instantiate via wasmhost, and register in
	// the live plugin registry. Without this, marketplace installs
	// don't survive a restart — the bundle stays on disk but the
	// plugin disappears from the running system.
	{
		stateRepo := db.NewPluginStateRepository(database)
		ids, err := pluginStore.List()
		if err != nil {
			log.Printf("Marketplace reload: list store: %v", err)
		}
		for _, pluginID := range ids {
			manifest, err := pluginStore.Manifest(pluginID)
			if err != nil {
				log.Printf("Marketplace reload: manifest %s: %v", pluginID, err)
				continue
			}
			wasmBytes, err := pluginStore.WASM(pluginID)
			if err != nil {
				log.Printf("Marketplace reload: wasm %s: %v", pluginID, err)
				continue
			}
			mod, err := wasmLoader.Load(context.Background(), pluginID, wasmBytes)
			if err != nil {
				log.Printf("Marketplace reload: instantiate %s: %v", pluginID, err)
				continue
			}
			// CallConfig builder threads the secrets store + capability
			// ceiling into every host import the plugin invokes. Without
			// this the wasm host_secrets_get path would return "not_found"
			// even for keys that exist under plugins/<plugin_id>/...
			pluginID := pluginID // capture for closure
			pluginCaps := manifest.Capabilities
			pluginAllowlist := manifest.Requires.NetworkAllowlist
			cfgBuilder := func() *wasmhost.CallConfig {
				return &wasmhost.CallConfig{
					PluginID:         pluginID,
					Capabilities:     pluginCaps,
					NetworkAllowlist: pluginAllowlist,
					Engine:           permEngine,
					Secrets:          secretStore,
					Tools:            toolExecutor,
				}
			}
			plug := wasmplugin.New(manifest, mod, cfgBuilder)
			if err := pluginRegistry.Register(plug); err != nil {
				log.Printf("Marketplace reload: register %s: %v", pluginID, err)
				_ = mod.Close(context.Background())
				continue
			}
			// If the persisted state row had Enabled=true, kick the
			// plugin live so the toggle's last value survives the
			// restart. New installs default to Enabled=false; users
			// flip them on in the UI.
			if st, err := stateRepo.Get(pluginID); err == nil && st.Enabled {
				_ = plug.Start(context.Background())
			}
			log.Printf("Reloaded marketplace plugin %s", pluginID)
		}
	}

	// Port + listener resolution. Bind eagerly so a busy port is detected
	// here (not deep inside http.Server.Serve), and so the tunnel + the
	// api.endpoint file can both reflect the *actual* port we're on.
	//
	// Persisted setting first, env override last (intended for test/CI
	// runners that must bind a free port without mutating the user's
	// stored preference). The persisted value still wins under normal
	// use because users expect the port they configured to stick.
	port := settingsRepo.GetOrDefault("api_port", "8080")
	if envPort := os.Getenv("NOMI_API_PORT"); envPort != "" {
		port = envPort
	}

	// Bind address: defaults to loopback (the desktop app convention)
	// because the API still trusts the local user account boundary.
	// Headless / docker deploys override via NOMI_BIND=0.0.0.0; the
	// auth token continues to gate every non-public request, so binding
	// to 0.0.0.0 doesn't open the API to anonymous callers — it just
	// makes the daemon reachable from sibling containers / the host.
	bindHost := os.Getenv("NOMI_BIND")
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}

	// Free-port fallback: if the configured port is occupied (another
	// nomid instance, an unrelated process on 8080, a leftover Docker
	// publish), we ask the kernel for any free port instead of dying.
	// The Tauri shell + nomi CLI both discover the URL via api.endpoint,
	// so a non-default port is transparent. NOMI_STRICT_PORT=1 disables
	// the fallback for users who'd rather see the failure (e.g. when a
	// reverse proxy assumes a fixed port).
	apiListener, err := net.Listen("tcp", bindHost+":"+port)
	if err != nil {
		strictPort := os.Getenv("NOMI_STRICT_PORT") == "1"
		if strictPort || !isAddrInUse(err) {
			log.Fatalf("Failed to bind API port %s: %v", port, err)
		}
		log.Printf("API port %s in use, picking a free port", port)
		apiListener, err = net.Listen("tcp", bindHost+":0")
		if err != nil {
			log.Fatalf("Failed to bind fallback port: %v", err)
		}
		port = strconv.Itoa(apiListener.Addr().(*net.TCPAddr).Port)
	}

	// Tunnel — expose local API to internet for inbound webhooks.
	// Disabled by default; enabled when the user configures an ngrok
	// authtoken in Settings → Connections.
	tunnelCfg := tunnel.Config{
		Kind:      tunnel.Kind(settingsRepo.GetOrDefault("tunnel_kind", string(tunnel.KindNoop))),
		AuthToken: settingsRepo.GetOrDefault("tunnel_auth_token", ""),
		Domain:    settingsRepo.GetOrDefault("tunnel_domain", ""),
	}
	var tunnelAdapter tunnel.Adapter
	if tunnelCfg.Kind != tunnel.KindNoop {
		var err error
		tunnelAdapter, err = tunnel.New(tunnelCfg)
		if err != nil {
			log.Printf("Tunnel init failed: %v — inbound webhooks disabled", err)
		} else {
			publicURL, err := tunnelAdapter.Start(context.Background(), "127.0.0.1:"+port)
			if err != nil {
				log.Printf("Tunnel start failed: %v — inbound webhooks disabled", err)
				tunnelAdapter = nil
			} else if publicURL != "" {
				log.Printf("Tunnel public URL: %s", publicURL)
				// Update all enabled connections that have webhooks enabled
				connRepo := db.NewConnectionRepository(database)
				conns, _ := connRepo.ListEnabled()
				for _, conn := range conns {
					if conn.WebhookEnabled {
						conn.WebhookURL = publicURL + "/webhooks/" + conn.PluginID + "/" + conn.ID
						_ = connRepo.Update(conn)
					}
				}
			}
		}
	}

	// API Server — bind to loopback only. Any listener beyond loopback would
	// expose the daemon (which can execute arbitrary commands) to the local
	// network, so this is a hard default, not a setting.
	router := api.NewRouter(api.RouterConfig{
		Runtime:        rt,
		DB:             database,
		EventBus:       eventBus,
		Approvals:      approvalMgr,
		Memory:         memManager,
		MemoryClient:   memClient,
		Tools:          toolRegistry,
		Connectors:     connRegistry,
		Plugins:        pluginRegistry,
		Secrets:        secretStore,
		AuthToken:      authToken,
		AuthTokenStore: api.NewTokenStore(authToken, tokenPath),
		Tunnel:         tunnelAdapter,
		PluginStore:     pluginStore,
		PluginVerifier:  pluginVerifier,
		WASMLoader:      wasmLoader,
		CatalogProvider: catalogProvider,
		PluginUpdater:   buildPluginUpdater(pluginRegistry, db.NewPluginStateRepository(database), pluginStore, pluginVerifier, wasmLoader, eventBus, catalogProvider),
		RemoteTemplates: db.NewRemoteTemplateRepository(database),
	})

	// Publish the endpoint so non-Go clients (the Tauri shell, e2e
	// harness) discover the URL instead of hardcoding 127.0.0.1:8080.
	// Lives next to auth.token in the data dir; same trust boundary.
	// Written *after* the listener bind above so a free-port fallback
	// is reflected accurately.
	apiURL := "http://" + bindHost + ":" + port
	endpointPath, err := api.WriteAPIEndpoint(dataDir, apiURL, port)
	if err != nil {
		log.Fatalf("Failed to publish API endpoint: %v", err)
	}
	log.Printf("API endpoint at %s (URL %s)", endpointPath, apiURL)

	server := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout intentionally left at zero: /events/stream is a
		// long-lived SSE endpoint. Non-streaming handlers return quickly.
	}

	// Start server in background. We Serve a pre-bound listener (rather
	// than ListenAndServe) so the port-in-use fallback above stays the
	// single source of truth for which port we're on.
	go func() {
		log.Printf("API server listening on %s", apiListener.Addr())
		if err := server.Serve(apiListener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Daily catalog poll → diff installed plugins, mark
	// available_version, emit plugin.update_available. First scan
	// happens 30s after boot so the daemon doesn't hammer the catalog
	// during startup; subsequent scans run on the configured interval
	// (default 24h). Off when the catalog provider isn't wired.
	if catalogProvider != nil {
		updateDeps := update.Deps{
			Registry: pluginRegistry,
			State:    db.NewPluginStateRepository(database),
			Store:    pluginStore,
			Verifier: pluginVerifier,
			Loader:   wasmLoader,
			Bus:      eventBus,
			Catalog:  catalogProvider,
		}
		pollInterval := 24 * time.Hour
		go func() {
			time.Sleep(30 * time.Second)
			t := time.NewTicker(pollInterval)
			defer t.Stop()
			for {
				if flagged, err := update.Scan(context.Background(), updateDeps); err != nil {
					log.Printf("Plugin update scan: %v", err)
				} else if flagged > 0 {
					log.Printf("Plugin update scan: %d plugin(s) have updates available", flagged)
				}
				<-t.C
			}
		}()
	}

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Cancel in-flight runs first so the goroutines unwind before we stop
	// connectors (which may be routing messages into the runtime) and the
	// HTTP server (which is streaming events to subscribers).
	rt.Shutdown()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := pluginRegistry.StopAll(); err != nil {
		log.Printf("Failed to stop some plugins: %v", err)
	}
	if err := connRegistry.StopAll(); err != nil {
		log.Printf("Error stopping connectors: %v", err)
	}
	if tunnelAdapter != nil {
		if err := tunnelAdapter.Stop(); err != nil {
			log.Printf("Tunnel stop: %v", err)
		}
	}

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	// Release the wasmhost runtime + its compiled-module cache. Last
	// because plugin Stop calls above may still touch wasm modules.
	if err := wasmLoader.Close(shutdownCtx); err != nil {
		log.Printf("wasm loader close: %v", err)
	}

	log.Println("Nomi Runtime stopped")
}

// buildPluginUpdater wires the per-request update closure that the
// API handler calls. Returns nil — leaving POST /plugins/:id/update
// returning 503 — when any required collaborator is missing
// (catalog provider absent / no marketplace root key).
func buildPluginUpdater(
	registry *plugins.Registry,
	state *db.PluginStateRepository,
	st *store.Store,
	verifier *signing.Verifier,
	loader *wasmhost.Loader,
	bus *events.EventBus,
	catalog func(ctx context.Context) (*hub.Catalog, error),
) func(ctx context.Context, pluginID string) (*domain.PluginState, error) {
	if catalog == nil || verifier == nil {
		return nil
	}
	deps := update.Deps{
		Registry:  registry,
		State:     state,
		Store:     st,
		Verifier:  verifier,
		Loader:    loader,
		Bus:       bus,
		Catalog:   catalog,
		HTTPFetch: defaultUpdateFetch,
	}
	return func(ctx context.Context, pluginID string) (*domain.PluginState, error) {
		return update.Update(ctx, deps, pluginID)
	}
}

// defaultUpdateFetch is the production bundle downloader used by the
// updater. Mirrors api.defaultHTTPFetch (kept separate to avoid
// importing the api package from here).
func defaultUpdateFetch(ctx context.Context, url string) ([]byte, error) {
	const maxBundle = 64 * 1024 * 1024
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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

// isAddrInUse reports whether err is a "port already in use" bind error.
// We unwrap through *net.OpError → *os.SyscallError to reach the raw
// syscall errno; this is the canonical Go check for EADDRINUSE and
// works the same on darwin/linux/windows (errors.Is handles the
// platform-specific WSAEADDRINUSE alias).
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE)
}
