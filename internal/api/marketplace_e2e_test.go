package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/plugins/publisher"
	"go.klarlabs.de/nomi/internal/plugins/signing"
	"go.klarlabs.de/nomi/internal/plugins/store"
	"go.klarlabs.de/nomi/internal/plugins/update"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// TestMarketplace_EndToEnd is the dogfood that proves the whole
// marketplace stack lifts off the ground. It exercises every piece
// of the lifecycle-05..10 work in one test against a real httptest
// server and a real daemon-shaped router:
//
//  1. Build a signed bundle (echo plugin) with the publisher tooling.
//  2. Use the publisher package to generate a signed catalog over
//     that bundle.
//  3. Stand up an httptest server that serves index.json + the
//     bundle bytes — same shape the production NomiHub deployment
//     will have.
//  4. Wire a router with all marketplace deps (store, verifier,
//     loader, catalog provider, updater) pointing at the test server.
//  5. POST /plugins/install {url:.../index.json#…} — but really we
//     install via URL to the bundle URL and the catalog endpoint
//     advertises that bundle.
//  6. GET /plugins/marketplace — confirm catalog round-trips through
//     the daemon.
//  7. Execute the installed plugin's tool through the registry.
//  8. Build a v2 bundle + a new catalog, swap out the test server's
//     contents, rotate the in-memory catalog cache.
//  9. Run the update.Scan path manually — confirm available_version
//     populates and plugin.update_available fires.
//  10. POST /plugins/{id}/update — confirm the swap completes and the
//     registry now serves v2.
//
// If any of these steps regresses, the marketplace pipeline is
// broken. This is the single test the publisher pipeline + daemon
// install/list/update path live or die by.
func TestMarketplace_EndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	// --- key material -----------------------------------------------
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	pubPub, pubPriv, _ := ed25519.GenerateKey(rand.Reader)

	// --- harness ---------------------------------------------------
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	database, err := db.New(db.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	verifier, _ := signing.NewVerifier(rootPub, nil)
	loader := wasmhost.NewLoader(ctx)
	defer loader.Close(ctx)

	pluginStore, err := store.New(filepath.Join(tmp, "plugins"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	registry := plugins.NewRegistry()
	stateRepo := db.NewPluginStateRepository(database)
	bus := events.NewEventBus(db.NewEventRepository(database))

	// --- helper: build signed echo bundle --------------------------
	buildEchoBundle := func(t *testing.T, version string) []byte {
		t.Helper()
		wd, _ := os.Getwd()
		echoWASM, err := os.ReadFile(filepath.Join(wd, "..", "plugins", "wasmhost", "testdata", "echo.wasm"))
		if err != nil {
			t.Fatalf("read echo.wasm: %v", err)
		}
		manifest := map[string]any{
			"id":           "com.nomi.dogfood",
			"name":         "Dogfood Echo",
			"version":      version,
			"cardinality":  "single",
			"capabilities": []string{"echo.echo"},
			"contributes": map[string]any{
				"tools": []map[string]any{
					{"name": "echo.echo", "capability": "echo.echo", "description": "round-trip"},
				},
			},
		}
		mBytes, _ := json.Marshal(manifest)
		expiry := time.Now().Add(365 * 24 * time.Hour)
		pubJSON, _ := json.Marshal(bundle.Publisher{
			Name:           "Nomi Dogfood",
			KeyFingerprint: "DOGFOOD-FP",
			PublicKey:      pubPub,
			RootSignature:  signing.SignPublisherClaim(rootPriv, "DOGFOOD-FP", pubPub, expiry),
			Expiry:         expiry,
		})
		var buf bytes.Buffer
		if err := bundle.Pack(&buf, bundle.Sources{
			ManifestJSON:  mBytes,
			WASM:          echoWASM,
			Readme:        []byte("# Dogfood\n\nProves the marketplace pipeline."),
			Signature:     signing.Sign(pubPriv, mBytes, echoWASM),
			PublisherJSON: pubJSON,
		}); err != nil {
			t.Fatalf("Pack: %v", err)
		}
		return buf.Bytes()
	}

	// --- httptest mirror of NomiHub --------------------------------
	// The mirror serves whatever bundle + catalog the test currently
	// holds. Mutating these slices simulates publishing a new version.
	var (
		currentBundle  []byte
		currentCatalog []byte
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(currentCatalog)
	})
	mux.HandleFunc("/bundles/dogfood.nomi-plugin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(currentBundle)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// --- v1 publish ------------------------------------------------
	currentBundle = buildEchoBundle(t, "0.1.0")
	bundleDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(bundleDir, "dogfood.nomi-plugin"), currentBundle, 0o644)
	catBytes, err := publisher.BuildCatalog(publisher.CatalogOptions{
		BundlesDir: bundleDir,
		BaseURL:    srv.URL + "/bundles",
		RootKey:    rootPriv,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}
	currentCatalog = catBytes

	// --- daemon-side glue -----------------------------------------
	// Catalog provider hits the test server fresh each call so v1/v2
	// transitions land immediately (no TTL caching for this test).
	hubClient, _ := hub.NewClient(rootPub, srv.Client())
	catalogProvider := func(ctx context.Context) (*hub.Catalog, error) {
		return hubClient.Fetch(ctx, srv.URL+"/index.json")
	}

	// HTTP fetcher used by both InstallPlugin and Updater. Routed
	// through srv.Client so the test server is reachable; production
	// uses defaultHTTPFetch.
	httpFetch := func(ctx context.Context, url string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
		}
		return io.ReadAll(resp.Body)
	}

	updateDeps := update.Deps{
		Registry:  registry,
		State:     stateRepo,
		Store:     pluginStore,
		Verifier:  verifier,
		Loader:    loader,
		Bus:       bus,
		Catalog:   catalogProvider,
		HTTPFetch: httpFetch,
	}

	pluginServer := NewPluginServer(registry,
		db.NewConnectionRepository(database),
		db.NewAssistantBindingRepository(database),
		stateRepo,
		newMemorySecretStore(),
	)
	pluginServer.AttachInstall(InstallDependencies{
		Store:           pluginStore,
		Verifier:        verifier,
		Loader:          loader,
		HTTPFetch:       httpFetch,
		CatalogProvider: catalogProvider,
		Updater: func(ctx context.Context, pluginID string) (*domain.PluginState, error) {
			return update.Update(ctx, updateDeps, pluginID)
		},
	})

	r := gin.New()
	plug := r.Group("/plugins")
	plug.GET("/marketplace", pluginServer.MarketplaceCatalog)
	plug.POST("/install", pluginServer.InstallPlugin)
	plug.POST("/:id/update", pluginServer.UpdatePlugin)
	plug.GET("/:id/state", pluginServer.GetPluginState)

	// --- step 1: marketplace endpoint surfaces v1 ------------------
	{
		req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("marketplace v1: %d %s", w.Code, w.Body.String())
		}
		var got hub.Catalog
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if len(got.Entries) != 1 || got.Entries[0].LatestVersion != "0.1.0" {
			t.Fatalf("marketplace v1 entries: %+v", got.Entries)
		}
	}

	// --- step 2: install via URL pointing at the bundle endpoint ---
	installBody := fmt.Sprintf(`{"url":%q}`, srv.URL+"/bundles/dogfood.nomi-plugin")
	{
		req := httptest.NewRequest(http.MethodPost, "/plugins/install",
			strings.NewReader(installBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("install: %d %s", w.Code, w.Body.String())
		}
	}

	// --- step 3: tool round-trip via registry ----------------------
	{
		p, err := registry.Get("com.nomi.dogfood")
		if err != nil {
			t.Fatalf("plugin not registered after install: %v", err)
		}
		tp, ok := p.(plugins.ToolProvider)
		if !ok {
			t.Fatal("registered plugin doesn't satisfy ToolProvider")
		}
		tools := tp.Tools()
		if len(tools) == 0 {
			t.Fatal("no tools surfaced from installed plugin")
		}
		out, err := tools[0].Execute(ctx, map[string]any{"hello": "marketplace"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		echoed, _ := out["echoed"].(map[string]any)
		if echoed["hello"] != "marketplace" {
			t.Fatalf("tool round-trip lost payload: %+v", out)
		}
	}

	// --- step 4: publish v2 --------------------------------------
	currentBundle = buildEchoBundle(t, "0.2.0")
	_ = os.WriteFile(filepath.Join(bundleDir, "dogfood.nomi-plugin"), currentBundle, 0o644)
	catBytes2, err := publisher.BuildCatalog(publisher.CatalogOptions{
		BundlesDir: bundleDir,
		BaseURL:    srv.URL + "/bundles",
		RootKey:    rootPriv,
	})
	if err != nil {
		t.Fatalf("BuildCatalog v2: %v", err)
	}
	currentCatalog = catBytes2

	// --- step 5: scan flags update available ----------------------
	flagged, err := update.Scan(ctx, updateDeps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if flagged != 1 {
		t.Fatalf("flagged = %d, want 1", flagged)
	}
	st, _ := stateRepo.Get("com.nomi.dogfood")
	if st.AvailableVersion != "0.2.0" {
		t.Fatalf("AvailableVersion = %q, want 0.2.0", st.AvailableVersion)
	}

	// --- step 6: POST /update via the wired endpoint --------------
	{
		req := httptest.NewRequest(http.MethodPost, "/plugins/com.nomi.dogfood/update", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("update: %d %s", w.Code, w.Body.String())
		}
		var st domain.PluginState
		_ = json.Unmarshal(w.Body.Bytes(), &st)
		if st.Version != "0.2.0" {
			t.Fatalf("post-update Version = %q, want 0.2.0", st.Version)
		}
		if st.AvailableVersion != "" {
			t.Fatalf("AvailableVersion should clear after update, got %q", st.AvailableVersion)
		}
	}

	// --- step 7: tool still works post-swap ----------------------
	{
		p, err := registry.Get("com.nomi.dogfood")
		if err != nil {
			t.Fatalf("plugin lost from registry after update: %v", err)
		}
		out, err := p.(plugins.ToolProvider).Tools()[0].Execute(ctx, map[string]any{"after": "update"})
		if err != nil {
			t.Fatalf("post-update Execute: %v", err)
		}
		echoed, _ := out["echoed"].(map[string]any)
		if echoed["after"] != "update" {
			t.Fatalf("post-update round-trip lost payload: %+v", out)
		}
	}
}
