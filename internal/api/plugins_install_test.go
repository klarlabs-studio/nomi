package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fmt"
	"github.com/gin-gonic/gin"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/plugins/signing"
	"go.klarlabs.de/nomi/internal/plugins/store"
	"go.klarlabs.de/nomi/internal/plugins/update"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// installHarness is a focused harness specifically for install/uninstall
// tests: minimal dependencies + key material + a wasmhost.Loader.
// Built separately from the smoke-test harness because that one doesn't
// know about plugin install dependencies and adding them there would
// touch every unrelated test.
type installHarness struct {
	router   *gin.Engine
	registry *plugins.Registry
	store    *store.Store
	rootPub  ed25519.PublicKey
	rootPriv ed25519.PrivateKey
	pubPub   ed25519.PublicKey
	pubPriv  ed25519.PrivateKey
	state    *db.PluginStateRepository
	wasmDir  string
	t        *testing.T
}

func newInstallHarness(t *testing.T) *installHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.New(db.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	pubPub, pubPriv, _ := ed25519.GenerateKey(rand.Reader)

	verifier, err := signing.NewVerifier(rootPub, nil)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	storeRoot := filepath.Join(dir, "plugins")
	st, err := store.New(storeRoot)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	loader := wasmhost.NewLoader(context.Background())
	t.Cleanup(func() { _ = loader.Close(context.Background()) })

	registry := plugins.NewRegistry()
	stateRepo := db.NewPluginStateRepository(database)
	connRepo := db.NewConnectionRepository(database)
	bindRepo := db.NewAssistantBindingRepository(database)
	secretStore := newMemorySecretStore()

	pluginServer := NewPluginServer(registry, connRepo, bindRepo, stateRepo, secretStore)
	pluginServer.AttachInstall(InstallDependencies{
		Store:    st,
		Verifier: verifier,
		Loader:   loader,
		// Simulated URL fetcher: tests pre-stage bundle bytes under
		// fake://<hash> and the harness serves them back. Avoids
		// hitting the real network.
		HTTPFetch: nil, // overridden per-test when URL path is exercised
	})

	r := gin.New()
	pluginGroup := r.Group("/plugins")
	pluginGroup.GET("", pluginServer.ListPlugins)
	pluginGroup.GET("/marketplace", pluginServer.MarketplaceCatalog)
	pluginGroup.POST("/install", pluginServer.InstallPlugin)
	pluginGroup.GET("/:id", pluginServer.GetPlugin)
	pluginGroup.GET("/:id/state", pluginServer.GetPluginState)
	pluginGroup.DELETE("/:id", pluginServer.UninstallPlugin)

	// Path to the existing TinyGo echo wasm we already build for the
	// wasmhost tests — reusing it instead of compiling fresh per test.
	wasmDir, _ := os.Getwd()
	return &installHarness{
		router:   r,
		registry: registry,
		store:    st,
		rootPub:  rootPub,
		rootPriv: rootPriv,
		pubPub:   pubPub,
		pubPriv:  pubPriv,
		state:    stateRepo,
		wasmDir:  filepath.Join(wasmDir, "..", "plugins", "wasmhost", "testdata"),
		t:        t,
	}
}

// echoWASMBytes returns the bytes of the TinyGo echo plugin used by the
// wasmhost tests. Centralized so individual tests don't repeat the
// path resolution.
func (h *installHarness) echoWASMBytes() []byte {
	body, err := os.ReadFile(filepath.Join(h.wasmDir, "echo.wasm"))
	if err != nil {
		h.t.Fatalf("read echo.wasm: %v", err)
	}
	return body
}

// makeBundle constructs a fully signed .nomi-plugin archive with the
// harness's keys + the given manifest id. mutate is called on the
// Sources before packing — tests that need an invalid bundle override
// e.g. Signature to garbage to exercise verifier failure paths.
func (h *installHarness) makeBundle(id string, mutate func(*bundle.Sources)) []byte {
	wasm := h.echoWASMBytes()
	manifest := map[string]any{
		"id":           id,
		"name":         "Test " + id,
		"version":      "0.1.0",
		"cardinality":  "single",
		"capabilities": []string{"echo.echo"},
		"contributes": map[string]any{
			"tools": []map[string]any{
				{"name": "echo.echo", "capability": "echo.echo", "description": "round-trip"},
			},
		},
	}
	manifestJSON, _ := json.Marshal(manifest)

	expiry := time.Now().Add(365 * 24 * time.Hour)
	pubJSON, _ := json.Marshal(bundle.Publisher{
		Name:           "Test Publisher",
		KeyFingerprint: "TEST-FP",
		PublicKey:      h.pubPub,
		RootSignature:  signing.SignPublisherClaim(h.rootPriv, "TEST-FP", h.pubPub, expiry),
		Expiry:         expiry,
	})

	src := bundle.Sources{
		ManifestJSON:  manifestJSON,
		WASM:          wasm,
		Readme:        []byte("# Test\n"),
		Signature:     signing.Sign(h.pubPriv, manifestJSON, wasm),
		PublisherJSON: pubJSON,
	}
	if mutate != nil {
		mutate(&src)
	}
	var buf bytes.Buffer
	if err := bundle.Pack(&buf, src); err != nil {
		h.t.Fatalf("Pack: %v", err)
	}
	return buf.Bytes()
}

// uploadInstall builds a multipart request for POST /plugins/install
// with the given bytes attached as the `bundle` field.
func (h *installHarness) uploadInstall(body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("bundle", "plugin.nomi-plugin")
	_, _ = io.Copy(part, bytes.NewReader(body))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/plugins/install", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// --- happy path ----------------------------------------------------

func TestInstall_HappyPath_Multipart(t *testing.T) {
	h := newInstallHarness(t)
	w := h.uploadInstall(h.makeBundle("com.test.echo1", nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp PluginResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Manifest.ID != "com.test.echo1" {
		t.Fatalf("manifest id = %q", resp.Manifest.ID)
	}
	if resp.State == nil || !resp.State.Installed || resp.State.Enabled {
		t.Fatalf("state = %+v, want installed=true enabled=false", resp.State)
	}
	if resp.State.Distribution != domain.PluginDistributionMarketplace {
		t.Fatalf("distribution = %q, want marketplace", resp.State.Distribution)
	}
	// Plugin should be live in the registry.
	if _, err := h.registry.Get("com.test.echo1"); err != nil {
		t.Fatalf("plugin not registered: %v", err)
	}
}

func TestInstall_RejectsBadSignature(t *testing.T) {
	h := newInstallHarness(t)
	body := h.makeBundle("com.test.bad", func(s *bundle.Sources) {
		// Tamper with the wasm AFTER signing — invalidates Signature.
		s.WASM = append(s.WASM, 0xFF)
	})
	w := h.uploadInstall(body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 from bad signature, got %d: %s", w.Code, w.Body.String())
	}
	// Nothing should have ended up on disk or in the registry.
	if _, err := h.registry.Get("com.test.bad"); err == nil {
		t.Fatal("plugin should not be registered after signature failure")
	}
	if _, err := h.store.WASM("com.test.bad"); err == nil {
		t.Fatal("plugin should not be on disk after signature failure")
	}
}

func TestInstall_RejectsDuplicate(t *testing.T) {
	h := newInstallHarness(t)
	bundle1 := h.makeBundle("com.test.dup", nil)
	if w := h.uploadInstall(bundle1); w.Code != http.StatusCreated {
		t.Fatalf("first install: %d %s", w.Code, w.Body.String())
	}
	w := h.uploadInstall(h.makeBundle("com.test.dup", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 on duplicate, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInstall_URLPath(t *testing.T) {
	h := newInstallHarness(t)
	body := h.makeBundle("com.test.url", nil)
	// Stub the fetcher to return our pre-built bundle bytes.
	h.injectFetcher(func(ctx context.Context, url string) ([]byte, error) {
		return body, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/plugins/install",
		bytes.NewBufferString(`{"url":"https://hub.example/plugin.nomi-plugin"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201 from URL install, got %d: %s", w.Code, w.Body.String())
	}
	st, _ := h.state.Get("com.test.url")
	if st == nil || st.SourceURL != "https://hub.example/plugin.nomi-plugin" {
		t.Fatalf("source_url not recorded: %+v", st)
	}
}

// --- uninstall -----------------------------------------------------

func TestUninstall_NonCascade(t *testing.T) {
	h := newInstallHarness(t)
	if w := h.uploadInstall(h.makeBundle("com.test.un1", nil)); w.Code != http.StatusCreated {
		t.Fatalf("install: %d %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodDelete, "/plugins/com.test.un1", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", w.Code, w.Body.String())
	}
	// Bundle gone, registry entry gone, but state row remains with
	// installed=false so a future reinstall reattaches connections.
	if _, err := h.store.WASM("com.test.un1"); err == nil {
		t.Fatal("bundle should be gone after uninstall")
	}
	if _, err := h.registry.Get("com.test.un1"); err == nil {
		t.Fatal("registry entry should be gone")
	}
	st, err := h.state.Get("com.test.un1")
	if err != nil || st == nil {
		t.Fatalf("state row should remain on non-cascade uninstall: err=%v state=%+v", err, st)
	}
	if st.Installed {
		t.Fatalf("state.Installed should be false after non-cascade, got %+v", st)
	}
}

func TestUninstall_Cascade(t *testing.T) {
	h := newInstallHarness(t)
	if w := h.uploadInstall(h.makeBundle("com.test.un2", nil)); w.Code != http.StatusCreated {
		t.Fatalf("install: %d %s", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/plugins/com.test.un2?cascade=true", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cascade uninstall: %d %s", w.Code, w.Body.String())
	}
	if st, err := h.state.Get("com.test.un2"); err == nil && st != nil {
		t.Fatalf("state row should be gone on cascade, got %+v", st)
	}
}

func TestUninstall_RefusesSystemPlugin(t *testing.T) {
	h := newInstallHarness(t)
	// Seed a system-distribution row directly. The handler must refuse
	// to uninstall regardless of whether it's currently registered.
	_ = h.state.EnsureSystemPlugin("com.test.system", "0.1.0")
	req := httptest.NewRequest(http.MethodDelete, "/plugins/com.test.system?cascade=true", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 for system plugin uninstall, got %d: %s", w.Code, w.Body.String())
	}
}

// --- marketplace catalog -------------------------------------------

func TestMarketplaceCatalog_503WhenNotConfigured(t *testing.T) {
	h := newInstallHarness(t)
	// Default harness wires install deps but not CatalogProvider.
	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 with no provider, got %d: %s", w.Code, w.Body.String())
	}
}

// --- update endpoint -----------------------------------------------

func TestUpdatePlugin_503WhenNotConfigured(t *testing.T) {
	h := newInstallHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/plugins/com.test.x/update", nil)
	w := httptest.NewRecorder()
	// Default install harness wires Updater=nil.
	h.router.POST("/plugins/:id/update", h.pluginServerWithUpdater(t, nil).UpdatePlugin)
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 with no updater, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePlugin_404OnUnknown(t *testing.T) {
	h := newInstallHarness(t)
	srv := h.pluginServerWithUpdater(t, func(ctx context.Context, id string) (*domain.PluginState, error) {
		return nil, fmt.Errorf("%w: %s", update.ErrPluginNotInstalled, id)
	})
	r := gin.New()
	r.POST("/plugins/:id/update", srv.UpdatePlugin)
	req := httptest.NewRequest(http.MethodPost, "/plugins/com.unknown/update", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown plugin, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePlugin_409OnAlreadyAtLatest(t *testing.T) {
	h := newInstallHarness(t)
	srv := h.pluginServerWithUpdater(t, func(ctx context.Context, id string) (*domain.PluginState, error) {
		return nil, update.ErrAlreadyAtLatest
	})
	r := gin.New()
	r.POST("/plugins/:id/update", srv.UpdatePlugin)
	req := httptest.NewRequest(http.MethodPost, "/plugins/com.x/update", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
}

// pluginServerWithUpdater returns a fresh PluginServer wired to call
// the given updater. Used by the update-endpoint tests so the harness
// itself stays uninvolved with update logic.
func (h *installHarness) pluginServerWithUpdater(t *testing.T, fn func(ctx context.Context, id string) (*domain.PluginState, error)) *PluginServer {
	t.Helper()
	srv := NewPluginServer(h.registry, db.NewConnectionRepository(stateDB(t, h.store)), nil, h.state, newMemorySecretStore())
	verifier, _ := signing.NewVerifier(h.rootPub, nil)
	loader := wasmhost.NewLoader(context.Background())
	t.Cleanup(func() { _ = loader.Close(context.Background()) })
	srv.AttachInstall(InstallDependencies{
		Store:    h.store,
		Verifier: verifier,
		Loader:   loader,
		Updater:  fn,
	})
	return srv
}

func TestMarketplaceCatalog_ReturnsCatalogWhenConfigured(t *testing.T) {
	h := newInstallHarness(t)
	stub := &hub.Catalog{
		SchemaVersion: hub.SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Entries: []hub.Entry{
			{PluginID: "com.example.x", Name: "X", LatestVersion: "0.1.0",
				Capabilities: []string{"network.outgoing"}, BundleURL: "https://hub/x.np"},
		},
	}
	// Re-attach install with the stub catalog provider.
	verifier, _ := signing.NewVerifier(h.rootPub, nil)
	loader := wasmhost.NewLoader(context.Background())
	t.Cleanup(func() { _ = loader.Close(context.Background()) })
	pluginServer := NewPluginServer(h.registry,
		db.NewConnectionRepository(stateDB(t, h.store)), nil, h.state, newMemorySecretStore())
	pluginServer.AttachInstall(InstallDependencies{
		Store:           h.store,
		Verifier:        verifier,
		Loader:          loader,
		CatalogProvider: func(ctx context.Context) (*hub.Catalog, error) { return stub, nil },
	})
	r := gin.New()
	r.GET("/plugins/marketplace", pluginServer.MarketplaceCatalog)
	req := httptest.NewRequest(http.MethodGet, "/plugins/marketplace", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var got hub.Catalog
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Entries) != 1 || got.Entries[0].PluginID != "com.example.x" {
		t.Fatalf("catalog round-trip lost entries: %+v", got)
	}
}

// injectFetcher swaps the install handler's HTTPFetch at runtime.
// Reaches through PluginServer.install — only safe in tests.
func (h *installHarness) injectFetcher(fn func(ctx context.Context, url string) ([]byte, error)) {
	// Find the PluginServer by walking the routes. Simpler: rebuild
	// the install dependencies. Since this test doesn't need to keep
	// the original deps it constructs fresh and re-attaches.
	verifier, _ := signing.NewVerifier(h.rootPub, nil)
	loader := wasmhost.NewLoader(context.Background())
	h.t.Cleanup(func() { _ = loader.Close(context.Background()) })

	pluginServer := NewPluginServer(
		h.registry,
		db.NewConnectionRepository(stateDB(h.t, h.store)),
		nil, h.state, newMemorySecretStore(),
	)
	pluginServer.AttachInstall(InstallDependencies{
		Store:     h.store,
		Verifier:  verifier,
		Loader:    loader,
		HTTPFetch: fn,
	})

	r := gin.New()
	r.POST("/plugins/install", pluginServer.InstallPlugin)
	r.DELETE("/plugins/:id", pluginServer.UninstallPlugin)
	r.GET("/plugins/:id/state", pluginServer.GetPluginState)
	h.router = r
}

// stateDB lifts the harness's underlying *db.DB out so the rebuilt
// PluginServer in injectFetcher reuses the same database. Not pretty
// but contained — the install harness is test-only.
func stateDB(t *testing.T, _ *store.Store) *db.DB {
	t.Helper()
	// The fetcher swap path doesn't actually need connection access
	// for the URL-install test, so an empty in-memory DB suffices.
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("memory db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatalf("memory migrate: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
