// nomi-publish is the catalog-building CLI for NomiHub. Given a
// directory of signed .nomi-plugin bundles + the root signing key +
// the base URL the bundles will be served from, it produces a signed
// index.json the daemon's hub.Client can consume.
//
// Today this tool ships only the `catalog` subcommand. Future
// subcommands (`sign-publisher`, `bundle`) will round out the
// publisher's flow; for v1 the bundle authors hand-craft those
// artifacts since the marketplace tier still has only one or two
// publishers.
//
// Usage:
//
//	nomi-publish catalog \
//	    -bundles ./bundles \
//	    -base-url https://hub.nomi.ai/bundles \
//	    -root-key ./root.priv \
//	    -out ./public/index.json
//
// The root-key file is the raw 64-byte ed25519 private key encoded
// as base64. Same format the daemon expects via
// NOMI_MARKETPLACE_ROOT_KEY (which carries the matching public key).
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"

	"go.klarlabs.de/nomi/internal/plugins/publisher"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "catalog":
		runCatalog(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runCatalog(args []string) {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	bundles := fs.String("bundles", "", "directory containing signed .nomi-plugin files")
	baseURL := fs.String("base-url", "", "base URL the bundles will be served from (no trailing slash)")
	rootKey := fs.String("root-key", "", "path to the base64-encoded ed25519 private root key")
	out := fs.String("out", "", "output path for the signed index.json")
	_ = fs.Parse(args)

	if *bundles == "" || *baseURL == "" || *rootKey == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}

	keyBytes, err := os.ReadFile(*rootKey)
	if err != nil {
		fail("read root-key: %v", err)
	}
	priv, err := base64.StdEncoding.DecodeString(string(keyBytes))
	if err != nil {
		fail("root-key not valid base64: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		fail("root-key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}

	bytes, err := publisher.BuildCatalog(publisher.CatalogOptions{
		BundlesDir: *bundles,
		BaseURL:    *baseURL,
		RootKey:    ed25519.PrivateKey(priv),
	})
	if err != nil {
		fail("build catalog: %v", err)
	}
	if err := os.WriteFile(*out, bytes, 0o600); err != nil {
		fail("write %s: %v", *out, err)
	}
	fmt.Printf("Wrote signed catalog to %s\n", *out)
}

func usage() {
	fmt.Fprintln(os.Stderr, `nomi-publish — NomiHub catalog publisher

Subcommands:

  catalog   Build a signed index.json from a directory of .nomi-plugin bundles.

Run "nomi-publish <subcommand> -h" for subcommand-specific flags.`)
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
