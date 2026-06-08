package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/plugins/signing"
	"go.klarlabs.de/nomi/internal/plugins/store"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/plugins/wasmplugin"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// updateFixture is everything the update tests need. Centralized so
// individual tests focus on the one knob they're exercising.
type updateFixture struct {
	deps      Deps
	rootPub   ed25519.PublicKey
	rootPriv  ed25519.PrivateKey
	pubPub    ed25519.PublicKey
	pubPriv   ed25519.PrivateKey
	pluginID  string
	wasmBytes []byte

	stateRepo *db.PluginStateRepository
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "test.db")
	database, err := db.New(db.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	pubPub, pubPriv, _ := ed25519.GenerateKey(rand.Reader)

	verifier, _ := signing.NewVerifier(rootPub, nil)
	loader := wasmhost.NewLoader(ctx)
	t.Cleanup(func() { _ = loader.Close(ctx) })

	stateRepo := db.NewPluginStateRepository(database)

	storeRoot := filepath.Join(tmp, "plugins")
	st, err := store.New(storeRoot)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	bus := events.NewEventBus(db.NewEventRepository(database))
	registry := plugins.NewRegistry()

	wd, _ := os.Getwd()
	wasmBytes, err := os.ReadFile(filepath.Join(wd, "..", "wasmhost", "testdata", "echo.wasm"))
	if err != nil {
		t.Fatalf("read echo.wasm: %v", err)
	}

	return &updateFixture{
		deps: Deps{
			Registry: registry,
			State:    stateRepo,
			Store:    st,
			Verifier: verifier,
			Loader:   loader,
			Bus:      bus,
			Now:      func() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) },
		},
		rootPub:   rootPub,
		rootPriv:  rootPriv,
		pubPub:    pubPub,
		pubPriv:   pubPriv,
		pluginID:  "com.test.echo",
		wasmBytes: wasmBytes,
		stateRepo: stateRepo,
	}
}

// makeBundle constructs a fully-signed bundle for the fixture's
// pluginID at the given version. Returned bytes are the gzipped tar
// suitable for either storing on disk or feeding into Update.
func (f *updateFixture) makeBundle(t *testing.T, version string) []byte {
	t.Helper()
	manifest := map[string]any{
		"id":           f.pluginID,
		"name":         "Echo",
		"version":      version,
		"cardinality":  "single",
		"capabilities": []string{"echo.echo"},
		"contributes": map[string]any{
			"tools": []map[string]any{
				{"name": "echo.echo", "capability": "echo.echo"},
			},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	expiry := time.Now().Add(365 * 24 * time.Hour)
	pBytes, _ := json.Marshal(bundle.Publisher{
		Name:           "Test",
		KeyFingerprint: "FP",
		PublicKey:      f.pubPub,
		RootSignature:  signing.SignPublisherClaim(f.rootPriv, "FP", f.pubPub, expiry),
		Expiry:         expiry,
	})
	src := bundle.Sources{
		ManifestJSON:  mBytes,
		WASM:          f.wasmBytes,
		Signature:     signing.Sign(f.pubPriv, mBytes, f.wasmBytes),
		PublisherJSON: pBytes,
	}
	var buf bytes.Buffer
	if err := bundle.Pack(&buf, src); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return buf.Bytes()
}

// install pretends the install handler ran already: writes the
// bundle to the store, registers the plugin, sets the state row.
// Tests that exercise Update from a clean install slate use this.
func (f *updateFixture) install(t *testing.T, version string) {
	t.Helper()
	bundleBytes := f.makeBundle(t, version)
	b, err := bundle.Open(bytes.NewReader(bundleBytes))
	if err != nil {
		t.Fatalf("bundle.Open: %v", err)
	}
	if err := f.deps.Store.Install(b); err != nil {
		t.Fatalf("store.Install: %v", err)
	}
	mod, err := f.deps.Loader.Load(context.Background(), b.Manifest.ID, b.WASM)
	if err != nil {
		t.Fatalf("loader.Load: %v", err)
	}
	plug := wasmplugin.New(b.Manifest, mod, nil)
	if err := f.deps.Registry.Register(plug); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	if err := f.stateRepo.Upsert(&domain.PluginState{
		PluginID:     f.pluginID,
		Distribution: domain.PluginDistributionMarketplace,
		Installed:    true,
		Enabled:      true,
		Version:      version,
		InstalledAt:  time.Now(),
	}); err != nil {
		t.Fatalf("state.Upsert: %v", err)
	}
}

// stubCatalog wires Deps.Catalog to return an entry pointing at
// canned bundle bytes (served by the stubFetch). bundleHash is what
// Update will compare against the entry.SHA256.
func (f *updateFixture) stubCatalog(bundleURL, bundleHash string, fetched *[]byte) {
	f.deps.Catalog = func(ctx context.Context) (*hub.Catalog, error) {
		return &hub.Catalog{
			SchemaVersion: hub.SchemaVersion,
			GeneratedAt:   time.Now().UTC(),
			Entries: []hub.Entry{{
				PluginID:      f.pluginID,
				LatestVersion: "0.2.0",
				BundleURL:     bundleURL,
				SHA256:        bundleHash,
				Capabilities:  []string{"echo.echo"},
			}},
		}, nil
	}
	f.deps.HTTPFetch = func(ctx context.Context, url string) ([]byte, error) {
		return *fetched, nil
	}
}

// --- Scan ----------------------------------------------------------

func TestScan_FlagsNewerVersion(t *testing.T) {
	f := newUpdateFixture(t)
	f.install(t, "0.1.0")
	bundleBytes := f.makeBundle(t, "0.2.0")
	f.stubCatalog("https://hub/echo-0.2.0.np", "irrelevant-for-scan", &bundleBytes)

	flagged, err := Scan(context.Background(), f.deps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if flagged != 1 {
		t.Fatalf("flagged = %d, want 1", flagged)
	}
	st, _ := f.stateRepo.Get(f.pluginID)
	if st.AvailableVersion != "0.2.0" {
		t.Fatalf("AvailableVersion = %q, want 0.2.0", st.AvailableVersion)
	}
	if st.LastCheckedAt == nil {
		t.Fatal("LastCheckedAt not set after Scan")
	}
}

func TestScan_ClearsStaleAvailable(t *testing.T) {
	// Plugin previously had AvailableVersion = 0.2.0; a later catalog
	// shows 0.2.0 is now installed (matching) → AvailableVersion clears.
	f := newUpdateFixture(t)
	f.install(t, "0.2.0")
	st, _ := f.stateRepo.Get(f.pluginID)
	st.AvailableVersion = "0.2.0"
	_ = f.stateRepo.Upsert(st)

	dummy := []byte{}
	f.stubCatalog("x", "x", &dummy)
	if _, err := Scan(context.Background(), f.deps); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	st, _ = f.stateRepo.Get(f.pluginID)
	if st.AvailableVersion != "" {
		t.Fatalf("AvailableVersion should clear, got %q", st.AvailableVersion)
	}
}

func TestScan_IsIdempotentForRepeatedFlags(t *testing.T) {
	// Running Scan twice with the same catalog should not emit a
	// second plugin.update_available — only the first transition
	// counts so subscribers don't see duplicate alerts.
	f := newUpdateFixture(t)
	f.install(t, "0.1.0")
	bundleBytes := f.makeBundle(t, "0.2.0")
	f.stubCatalog("x", "x", &bundleBytes)

	first, _ := Scan(context.Background(), f.deps)
	second, _ := Scan(context.Background(), f.deps)
	if first != 1 || second != 0 {
		t.Fatalf("first=%d second=%d (want 1, 0)", first, second)
	}
}

// --- Update --------------------------------------------------------

func TestUpdate_HappyPath(t *testing.T) {
	f := newUpdateFixture(t)
	f.install(t, "0.1.0")

	newBundle := f.makeBundle(t, "0.2.0")
	b, _ := bundle.Open(bytes.NewReader(newBundle))
	f.stubCatalog("https://hub/echo-0.2.0.np", b.Hash, &newBundle)

	st, err := Update(context.Background(), f.deps, f.pluginID)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if st.Version != "0.2.0" {
		t.Fatalf("Version = %q, want 0.2.0", st.Version)
	}
	if st.AvailableVersion != "" {
		t.Fatalf("AvailableVersion should be cleared post-update, got %q", st.AvailableVersion)
	}
	// Plugin should still be live in the registry, executing the new
	// version (we can't easily check version via the wasm but at least
	// confirm the plugin is registered).
	if _, err := f.deps.Registry.Get(f.pluginID); err != nil {
		t.Fatalf("plugin lost from registry after update: %v", err)
	}
}

func TestUpdate_AbortsOnSignatureFailure(t *testing.T) {
	f := newUpdateFixture(t)
	f.install(t, "0.1.0")

	// Build a properly-shaped bundle but tamper with WASM after
	// signing — Verify must reject and Update must leave the prior
	// version installed and live.
	newBundle := f.makeBundle(t, "0.2.0")
	// Decompress, mutate, ... or just append garbage which breaks gzip:
	// simpler — pass a shape that bundle.Open accepts but signing rejects.
	// We do this by building a bundle and re-packing with a corrupted wasm.
	tampered := f.makeBundleWithTamper(t, "0.2.0")
	b, _ := bundle.Open(bytes.NewReader(newBundle))
	f.stubCatalog("https://hub/echo-0.2.0.np", b.Hash, &tampered)

	_, err := Update(context.Background(), f.deps, f.pluginID)
	if err == nil {
		t.Fatal("expected Update to fail on signature mismatch")
	}
	st, _ := f.stateRepo.Get(f.pluginID)
	if st.Version != "0.1.0" {
		t.Fatalf("Version drifted after failed update: %q", st.Version)
	}
	if !st.Installed {
		t.Fatalf("Installed flag flipped after failed update: %+v", st)
	}
	if _, err := f.deps.Registry.Get(f.pluginID); err != nil {
		t.Fatalf("registry lost plugin after failed update: %v", err)
	}
}

func TestUpdate_AbortsOnHashMismatch(t *testing.T) {
	f := newUpdateFixture(t)
	f.install(t, "0.1.0")
	newBundle := f.makeBundle(t, "0.2.0")
	// Catalog claims a different SHA256.
	f.stubCatalog("x", "deadbeef-not-the-real-hash", &newBundle)
	_, err := Update(context.Background(), f.deps, f.pluginID)
	if !errors.Is(err, ErrBundleHashMismatch) {
		t.Fatalf("want ErrBundleHashMismatch, got %v", err)
	}
}

func TestUpdate_AbortsOnAlreadyAtLatest(t *testing.T) {
	f := newUpdateFixture(t)
	f.install(t, "0.2.0")
	newBundle := f.makeBundle(t, "0.2.0")
	b, _ := bundle.Open(bytes.NewReader(newBundle))
	f.stubCatalog("x", b.Hash, &newBundle)
	_, err := Update(context.Background(), f.deps, f.pluginID)
	if !errors.Is(err, ErrAlreadyAtLatest) {
		t.Fatalf("want ErrAlreadyAtLatest, got %v", err)
	}
}

func TestUpdate_RefusesSystemPlugin(t *testing.T) {
	f := newUpdateFixture(t)
	_ = f.stateRepo.EnsureSystemPlugin(f.pluginID, "0.1.0")
	dummy := []byte{}
	f.stubCatalog("x", "x", &dummy)
	_, err := Update(context.Background(), f.deps, f.pluginID)
	if !errors.Is(err, ErrSystemPluginRefused) {
		t.Fatalf("want ErrSystemPluginRefused, got %v", err)
	}
}

func TestUpdate_NotInstalled(t *testing.T) {
	f := newUpdateFixture(t)
	dummy := []byte{}
	f.stubCatalog("x", "x", &dummy)
	_, err := Update(context.Background(), f.deps, "com.never.installed")
	if !errors.Is(err, ErrPluginNotInstalled) {
		t.Fatalf("want ErrPluginNotInstalled, got %v", err)
	}
}

// --- compareSemver -------------------------------------------------

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.2.0", "0.1.9", 1},
		{"1.0.0", "0.99.0", 1},
		{"0.1.0", "0.1.1", -1},
		{"1.2.3", "1.2.3", 0},
		{"1.2", "1.2.0", 0}, // missing segments treated as zero
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// makeBundleWithTamper returns a bundle whose WASM was mutated AFTER
// the publisher signed it — passes structural bundle.Open but fails
// signing.Verify. Used by the signature-failure test.
func (f *updateFixture) makeBundleWithTamper(t *testing.T, version string) []byte {
	t.Helper()
	manifest := map[string]any{
		"id":           f.pluginID,
		"name":         "Echo",
		"version":      version,
		"cardinality":  "single",
		"capabilities": []string{"echo.echo"},
		"contributes": map[string]any{
			"tools": []map[string]any{{"name": "echo.echo", "capability": "echo.echo"}},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	expiry := time.Now().Add(365 * 24 * time.Hour)
	pBytes, _ := json.Marshal(bundle.Publisher{
		Name:           "Test",
		KeyFingerprint: "FP",
		PublicKey:      f.pubPub,
		RootSignature:  signing.SignPublisherClaim(f.rootPriv, "FP", f.pubPub, expiry),
		Expiry:         expiry,
	})
	// Sign over the ORIGINAL wasm…
	sig := signing.Sign(f.pubPriv, mBytes, f.wasmBytes)
	// …then ship a different wasm in the bundle.
	tamperedWASM := append([]byte{}, f.wasmBytes...)
	tamperedWASM = append(tamperedWASM, 0xFF)
	src := bundle.Sources{
		ManifestJSON:  mBytes,
		WASM:          tamperedWASM,
		Signature:     sig,
		PublisherJSON: pBytes,
	}
	var buf bytes.Buffer
	if err := bundle.Pack(&buf, src); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return buf.Bytes()
}
