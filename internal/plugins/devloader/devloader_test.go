package devloader

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
)

func writeBundle(t *testing.T, dir, name, pluginID string) {
	t.Helper()
	wasmPath := filepath.Join(wasmhostTestdataDir(t), "echo.wasm")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read echo.wasm: %v", err)
	}
	manifest := map[string]any{
		"id":           pluginID,
		"name":         "Dev " + pluginID,
		"version":      "0.0.1",
		"cardinality":  "single",
		"capabilities": []string{"echo.echo"},
		"contributes": map[string]any{
			"tools": []map[string]any{{"name": "echo.echo", "capability": "echo.echo"}},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	pBytes, _ := json.Marshal(bundle.Publisher{Name: "Dev", KeyFingerprint: "DEV"})

	var buf bytes.Buffer
	if err := bundle.Pack(&buf, bundle.Sources{
		ManifestJSON:  mBytes,
		WASM:          wasm,
		Signature:     bytes.Repeat([]byte{0x00}, 64),
		PublisherJSON: pBytes,
	}); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func wasmhostTestdataDir(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "wasmhost", "testdata")
}

func TestLoad_PicksUpBundleFiles(t *testing.T) {
	ctx := context.Background()
	loader := wasmhost.NewLoader(ctx)
	defer loader.Close(ctx)

	dir := t.TempDir()
	writeBundle(t, dir, "alpha.nomi-plugin", "com.dev.alpha")
	writeBundle(t, dir, "beta.nomi-plugin", "com.dev.beta")
	// Files that should be ignored:
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignore me"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "stale.wasm"), []byte("\x00asm"), 0o644)

	res, err := Load(ctx, dir, loader)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Plugins) != 2 {
		t.Fatalf("loaded %d plugins, want 2 (errors=%v)", len(res.Plugins), res.Errors)
	}
	// Stable lexical order.
	if res.Plugins[0].Manifest.ID != "com.dev.alpha" {
		t.Fatalf("plugin[0] = %q, want com.dev.alpha", res.Plugins[0].Manifest.ID)
	}
	if res.Plugins[1].Manifest.ID != "com.dev.beta" {
		t.Fatalf("plugin[1] = %q, want com.dev.beta", res.Plugins[1].Manifest.ID)
	}
	for _, p := range res.Plugins {
		_ = p.Plugin.Stop()
	}
}

func TestLoad_PartialFailureCollectsErrors(t *testing.T) {
	ctx := context.Background()
	loader := wasmhost.NewLoader(ctx)
	defer loader.Close(ctx)

	dir := t.TempDir()
	writeBundle(t, dir, "good.nomi-plugin", "com.dev.good")
	// Truncated bundle — bundle.Open will fail.
	_ = os.WriteFile(filepath.Join(dir, "broken.nomi-plugin"), []byte("not a bundle"), 0o644)

	res, err := Load(ctx, dir, loader)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Plugins) != 1 {
		t.Fatalf("plugins = %d, want 1", len(res.Plugins))
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %d, want 1", len(res.Errors))
	}
	for _, p := range res.Plugins {
		_ = p.Plugin.Stop()
	}
}

func TestLoad_CreatesMissingDir(t *testing.T) {
	ctx := context.Background()
	loader := wasmhost.NewLoader(ctx)
	defer loader.Close(ctx)

	dir := filepath.Join(t.TempDir(), "plugins-dev")
	res, err := Load(ctx, dir, loader)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Plugins) != 0 || len(res.Errors) != 0 {
		t.Fatalf("empty dir should yield empty result, got %+v", res)
	}
	// The dir should now exist so the boot path can write into it.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestLoad_NilLoaderRejected(t *testing.T) {
	_, err := Load(context.Background(), t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected nil-loader rejection")
	}
}
