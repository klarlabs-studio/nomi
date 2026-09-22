// Command gen builds a local signed NomiHub-shaped catalog from the
// in-tree echo.wasm fixture. DEV ONLY — writes a throwaway root key
// into dist/ alongside index.json + the .nomi-plugin bundle.
//
//	go run ./examples/wasm-marketplace-catalog/gen
//	# then: python3 -m http.server -d examples/wasm-marketplace-catalog/dist 8765
//	# NOMI_MARKETPLACE_ROOT_KEY=$(cat …/dist/root.pub.b64) nomid
//	# Settings → Plugins → Browse marketplace → Catalog URL = http://127.0.0.1:8765/index.json
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/publisher"
	"go.klarlabs.de/nomi/internal/plugins/signing"
)

func main() {
	_, thisFile, _, _ := runtime.Caller(0)
	exampleDir := filepath.Dir(filepath.Dir(thisFile)) // …/wasm-marketplace-catalog
	repoRoot := filepath.Dir(filepath.Dir(exampleDir)) // repo root
	wasmPath := filepath.Join(repoRoot, "internal", "plugins", "wasmhost", "testdata", "echo.wasm")
	wasm, err := os.ReadFile(wasmPath) //nolint:gosec // G304: fixed repo path
	if err != nil {
		fail("read echo.wasm: %v", err)
	}

	outDir := filepath.Join(exampleDir, "dist")
	bundlesDir := filepath.Join(outDir, "bundles")
	if err := os.MkdirAll(bundlesDir, 0o755); err != nil {
		fail("mkdir: %v", err)
	}

	rootPub, rootPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("root keygen: %v", err)
	}
	pubPub, pubPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("publisher keygen: %v", err)
	}

	manifest := map[string]any{
		"id":           "com.nomi.examples.echo",
		"name":         "Echo (example)",
		"version":      "0.1.0",
		"author":       "Nomi Examples",
		"description":  "Curated marketplace example — round-trips a string. Shows signed WASM install end-to-end.",
		"cardinality":  "single",
		"capabilities": []string{"echo.echo"},
		"contributes": map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo.echo",
					"capability":  "echo.echo",
					"description": "Echo the input string back.",
				},
			},
		},
	}
	mBytes, _ := json.Marshal(manifest)
	expiry := time.Now().Add(10 * 365 * 24 * time.Hour)
	fp := "EXAMPLE-ECHO-PUB"
	pubJSON, _ := json.Marshal(bundle.Publisher{
		Name:           "Nomi Examples",
		KeyFingerprint: fp,
		PublicKey:      pubPub,
		RootSignature:  signing.SignPublisherClaim(rootPriv, fp, pubPub, expiry),
		Expiry:         expiry,
	})
	var packed bytes.Buffer
	if err := bundle.Pack(&packed, bundle.Sources{
		ManifestJSON:  mBytes,
		WASM:          wasm,
		Readme:        []byte("# Echo example\n\nSigned marketplace fixture built from `echo.wasm`.\n"),
		Signature:     signing.Sign(pubPriv, mBytes, wasm),
		PublisherJSON: pubJSON,
	}); err != nil {
		fail("pack: %v", err)
	}
	bundleName := "echo.nomi-plugin"
	if err := os.WriteFile(filepath.Join(bundlesDir, bundleName), packed.Bytes(), 0o644); err != nil {
		fail("write bundle: %v", err)
	}

	baseURL := os.Getenv("NOMI_EXAMPLE_CATALOG_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8765/bundles"
	}
	catBytes, err := publisher.BuildCatalog(publisher.CatalogOptions{
		BundlesDir: bundlesDir,
		BaseURL:    baseURL,
		RootKey:    rootPriv,
	})
	if err != nil {
		fail("catalog: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "index.json"), catBytes, 0o644); err != nil {
		fail("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "root.pub.b64"),
		[]byte(base64.StdEncoding.EncodeToString(rootPub)+"\n"), 0o644); err != nil {
		fail("write root.pub: %v", err)
	}
	// Private key kept local for re-signing — never for production.
	if err := os.WriteFile(filepath.Join(outDir, "root.priv.b64"),
		[]byte(base64.StdEncoding.EncodeToString(rootPriv)+"\n"), 0o600); err != nil {
		fail("write root.priv: %v", err)
	}

	fmt.Printf("Wrote example catalog to %s\n", outDir)
	fmt.Println()
	fmt.Println("Next:")
	fmt.Printf("  python3 -m http.server -d %s 8765\n", outDir)
	fmt.Println("  export NOMI_MARKETPLACE_ROOT_KEY=$(tr -d '\\n' < " + filepath.Join(outDir, "root.pub.b64") + ")")
	fmt.Println("  # start nomid, then set Catalog URL to http://127.0.0.1:8765/index.json")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
