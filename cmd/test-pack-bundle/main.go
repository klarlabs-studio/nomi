// Test-only helper for the E2E install/uninstall walk: takes a wasm
// path + root private key (base64 via ROOT_PRIV env var), emits a
// signed .nomi-plugin to stdout. Lives under cmd/ so it can import
// internal/plugins/...; not shipped in the production binary set.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/signing"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: test-pack-bundle <wasm-path>")
		os.Exit(2)
	}
	root, err := base64.StdEncoding.DecodeString(os.Getenv("ROOT_PRIV"))
	if err != nil || len(root) != ed25519.PrivateKeySize {
		fmt.Fprintln(os.Stderr, "ROOT_PRIV must be a base64 ed25519 private key")
		os.Exit(1)
	}
	wasm, err := os.ReadFile(os.Args[1]) //nolint:gosec // G703: dev CLI tool; path is the user-supplied argv
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pubPub, pubPriv, _ := ed25519.GenerateKey(nil)
	manifest := map[string]any{
		"id":           "com.e2e.echo",
		"name":         "E2E Echo",
		"version":      "0.1.0",
		"author":       "E2E Test",
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
		Name:           "E2E Publisher",
		KeyFingerprint: "E2E-FP",
		PublicKey:      pubPub,
		RootSignature:  signing.SignPublisherClaim(ed25519.PrivateKey(root), "E2E-FP", pubPub, expiry),
		Expiry:         expiry,
	})

	var buf bytes.Buffer
	if err := bundle.Pack(&buf, bundle.Sources{
		ManifestJSON:  mBytes,
		WASM:          wasm,
		Readme:        []byte("# E2E Echo\n\nLive E2E install test."),
		Signature:     signing.Sign(pubPriv, mBytes, wasm),
		PublisherJSON: pubJSON,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, _ = os.Stdout.Write(buf.Bytes())
	fmt.Fprintf(os.Stderr, "wrote %d bytes\n", buf.Len())
}
