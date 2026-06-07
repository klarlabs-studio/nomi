package publisher

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/plugins/signing"
)

// fixturePublisher returns the keys + a helper that writes a signed
// bundle to disk under the given dir, ready for BuildCatalog to find.
type fixturePublisher struct {
	rootPub  ed25519.PublicKey
	rootPriv ed25519.PrivateKey
	pubPub   ed25519.PublicKey
	pubPriv  ed25519.PrivateKey
}

func newFixture(t *testing.T) *fixturePublisher {
	t.Helper()
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	pubPub, pubPriv, _ := ed25519.GenerateKey(rand.Reader)
	return &fixturePublisher{rootPub, rootPriv, pubPub, pubPriv}
}

func (f *fixturePublisher) writeBundle(t *testing.T, dir, filename, pluginID, version string) {
	t.Helper()
	manifest := map[string]any{
		"id":           pluginID,
		"name":         "Plugin " + pluginID,
		"version":      version,
		"author":       "Tests",
		"cardinality":  "single",
		"capabilities": []string{"echo.echo"},
		"contributes": map[string]any{
			"tools": []map[string]any{
				{"name": "echo.echo", "capability": "echo.echo"},
			},
		},
		"requires": map[string]any{
			"network_allowlist": []string{"api.example.com"},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	wasm := []byte("\x00asm\x01\x00\x00\x00FAKE")
	expiry := time.Now().Add(365 * 24 * time.Hour)
	pubJSON, _ := json.Marshal(bundle.Publisher{
		Name:           "Test Publisher",
		KeyFingerprint: "FP-FOR-" + pluginID,
		PublicKey:      f.pubPub,
		RootSignature:  signing.SignPublisherClaim(f.rootPriv, "FP-FOR-"+pluginID, f.pubPub, expiry),
		Expiry:         expiry,
	})
	src := bundle.Sources{
		ManifestJSON:  mBytes,
		WASM:          wasm,
		Readme:        []byte("# " + pluginID + "\n\nThis is the long-form description that exceeds the excerpt cap..."),
		Signature:     signing.Sign(f.pubPriv, mBytes, wasm),
		PublisherJSON: pubJSON,
	}
	var buf bytes.Buffer
	if err := bundle.Pack(&buf, src); err != nil {
		t.Fatalf("Pack %s: %v", pluginID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func TestBuildCatalog_HappyPath(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.writeBundle(t, dir, "alpha.nomi-plugin", "com.test.alpha", "0.1.0")
	f.writeBundle(t, dir, "beta.nomi-plugin", "com.test.beta", "0.2.0")

	out, err := BuildCatalog(CatalogOptions{
		BundlesDir: dir,
		BaseURL:    "https://hub.example/bundles",
		RootKey:    f.rootPriv,
	})
	if err != nil {
		t.Fatalf("BuildCatalog: %v", err)
	}

	// Round-trip through the daemon's hub.Client to confirm the
	// signed envelope is well-formed and the schema matches what the
	// daemon actually parses.
	client, _ := hub.NewClient(f.rootPub, nil)
	cat, err := client.Parse(out)
	if err != nil {
		t.Fatalf("hub.Client.Parse on publisher output: %v", err)
	}
	if len(cat.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(cat.Entries))
	}
	// Lexical filename order — alpha first.
	if cat.Entries[0].PluginID != "com.test.alpha" {
		t.Fatalf("entry[0] id = %q, want com.test.alpha", cat.Entries[0].PluginID)
	}
	if cat.Entries[0].BundleURL != "https://hub.example/bundles/alpha.nomi-plugin" {
		t.Fatalf("entry[0] BundleURL = %q", cat.Entries[0].BundleURL)
	}
	if cat.Entries[0].SHA256 == "" {
		t.Fatal("entry[0] SHA256 missing — daemon update flow needs this")
	}
	if cat.Entries[0].InstallSizeBytes <= 0 {
		t.Fatalf("entry[0] InstallSizeBytes = %d", cat.Entries[0].InstallSizeBytes)
	}
	if cat.Entries[0].PublisherFingerprint != "FP-FOR-com.test.alpha" {
		t.Fatalf("entry[0] PublisherFingerprint = %q", cat.Entries[0].PublisherFingerprint)
	}
	if cat.Entries[0].NetworkAllowlist[0] != "api.example.com" {
		t.Fatalf("entry[0] NetworkAllowlist drift: %v", cat.Entries[0].NetworkAllowlist)
	}
}

func TestBuildCatalog_RejectsDuplicatePluginID(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.writeBundle(t, dir, "v1.nomi-plugin", "com.test.dup", "0.1.0")
	f.writeBundle(t, dir, "v2.nomi-plugin", "com.test.dup", "0.2.0")

	_, err := BuildCatalog(CatalogOptions{
		BundlesDir: dir,
		BaseURL:    "https://hub.example/bundles",
		RootKey:    f.rootPriv,
	})
	if err == nil {
		t.Fatal("expected duplicate plugin id rejection")
	}
}

func TestBuildCatalog_RejectsCorruptBundle(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.writeBundle(t, dir, "good.nomi-plugin", "com.test.good", "0.1.0")
	_ = os.WriteFile(filepath.Join(dir, "broken.nomi-plugin"), []byte("not a bundle"), 0o644)

	_, err := BuildCatalog(CatalogOptions{
		BundlesDir: dir,
		BaseURL:    "https://hub.example/bundles",
		RootKey:    f.rootPriv,
	})
	if err == nil {
		t.Fatal("expected corrupt bundle to fail the build (publishers must fix before shipping)")
	}
}

func TestBuildCatalog_DeterministicForReproducibleBuilds(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.writeBundle(t, dir, "alpha.nomi-plugin", "com.test.alpha", "0.1.0")

	frozen := func() time.Time { return time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC) }
	a, _ := BuildCatalog(CatalogOptions{
		BundlesDir:  dir,
		BaseURL:     "https://hub.example/bundles",
		RootKey:     f.rootPriv,
		GeneratedAt: frozen,
	})
	b, _ := BuildCatalog(CatalogOptions{
		BundlesDir:  dir,
		BaseURL:     "https://hub.example/bundles",
		RootKey:     f.rootPriv,
		GeneratedAt: frozen,
	})
	if !bytes.Equal(a, b) {
		t.Fatal("publisher output is not deterministic — would defeat content-addressed caching downstream")
	}
}
