// Package publisher is the catalog-build half of the marketplace
// pipeline. It scans a directory of signed .nomi-plugin bundles,
// extracts catalog-shaped metadata from each, and emits a signed
// index.json the daemon's hub.Client consumes.
//
// The CLI in cmd/nomi-publish/ is the user-facing wrapper; this
// package holds the logic so the integration tests can invoke
// BuildCatalog directly without spinning up a subprocess.
package publisher

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
)

// CatalogOptions is the BuildCatalog input.
type CatalogOptions struct {
	// BundlesDir is the on-disk directory holding *.nomi-plugin files.
	// Each file becomes one Entry in the resulting catalog.
	BundlesDir string
	// BaseURL is the URL prefix the daemon will see when downloading
	// bundles — bundle URLs in the catalog are formed as
	//   <base-url>/<filename>
	// so the publisher can host bundles flat under one path on
	// GitHub Pages without per-bundle metadata.
	BaseURL string
	// RootKey signs the catalog envelope. Must match the public key
	// the daemon trusts via NOMI_MARKETPLACE_ROOT_KEY.
	RootKey ed25519.PrivateKey
	// GeneratedAt overrides the timestamp on the catalog. nil =
	// time.Now. Tests inject a frozen value for reproducible output.
	GeneratedAt func() time.Time
}

// BuildCatalog scans BundlesDir, parses each bundle, derives an
// Entry, and signs the resulting Catalog with RootKey. Returns the
// SignedCatalog wire bytes.
func BuildCatalog(opts CatalogOptions) ([]byte, error) {
	if opts.BundlesDir == "" {
		return nil, fmt.Errorf("publisher: BundlesDir required")
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("publisher: BaseURL required")
	}
	if len(opts.RootKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("publisher: RootKey is %d bytes, want %d", len(opts.RootKey), ed25519.PrivateKeySize)
	}
	now := opts.GeneratedAt
	if now == nil {
		now = time.Now
	}

	entries, err := scanBundles(opts.BundlesDir, opts.BaseURL)
	if err != nil {
		return nil, err
	}
	cat := hub.Catalog{
		SchemaVersion: hub.SchemaVersion,
		GeneratedAt:   now().UTC(),
		Entries:       entries,
	}
	body, err := json.Marshal(cat)
	if err != nil {
		return nil, fmt.Errorf("publisher: marshal catalog: %w", err)
	}
	return hub.SignCatalog(opts.RootKey, body)
}

// scanBundles enumerates *.nomi-plugin files under dir, parses each,
// and projects metadata into a hub.Entry. Bundles that fail to parse
// are returned as errors — callers should fix them rather than
// publish a partial catalog.
func scanBundles(dir, baseURL string) ([]hub.Entry, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("publisher: read %s: %w", dir, err)
	}
	var names []string
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".nomi-plugin") {
			continue
		}
		names = append(names, f.Name())
	}
	sort.Strings(names) // stable catalog ordering for reproducible builds

	out := make([]hub.Entry, 0, len(names))
	seen := map[string]string{} // plugin_id -> file
	for _, name := range names {
		path := filepath.Join(dir, name)
		entry, err := bundleToEntry(path, baseURL+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("publisher: %s: %w", name, err)
		}
		if prev, dup := seen[entry.PluginID]; dup {
			return nil, fmt.Errorf("publisher: plugin %q appears in both %s and %s — pick one before publishing",
				entry.PluginID, prev, name)
		}
		seen[entry.PluginID] = name
		out = append(out, entry)
	}
	return out, nil
}

// bundleToEntry opens one bundle file and derives the catalog row.
// SHA256 is computed over the gzipped tar bytes (same definition
// bundle.Open populates as Bundle.Hash) so the daemon can confirm a
// downloaded blob matches the catalog claim before installing.
func bundleToEntry(path, bundleURL string) (hub.Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return hub.Entry{}, fmt.Errorf("read: %w", err)
	}
	b, err := bundle.Open(strings.NewReader(string(raw)))
	if err != nil {
		return hub.Entry{}, fmt.Errorf("parse: %w", err)
	}
	hash := sha256.Sum256(raw)
	return hub.Entry{
		PluginID:             b.Manifest.ID,
		Name:                 b.Manifest.Name,
		LatestVersion:        b.Manifest.Version,
		Author:               b.Manifest.Author,
		Description:          b.Manifest.Description,
		Capabilities:         b.Manifest.Capabilities,
		NetworkAllowlist:     b.Manifest.Requires.NetworkAllowlist,
		InstallSizeBytes:     int64(len(raw)),
		SHA256:               hex.EncodeToString(hash[:]),
		BundleURL:            bundleURL,
		PublisherFingerprint: b.Publisher.KeyFingerprint,
		ReadmeExcerpt:        readmeExcerpt(b.Readme),
	}, nil
}

// readmeExcerpt returns the first paragraph of the README so the
// install dialog can preview without forcing a full download.
// Caps at 280 chars to keep the catalog small.
func readmeExcerpt(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	text := strings.TrimSpace(string(body))
	if i := strings.Index(text, "\n\n"); i > 0 {
		text = text[:i]
	}
	if len(text) > 280 {
		text = strings.TrimSpace(text[:280]) + "…"
	}
	return text
}
