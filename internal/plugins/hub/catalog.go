// Package hub is the daemon-side client for the NomiHub plugin
// catalog (ADR 0002 §2 / lifecycle-09). The catalog is a single
// signed JSON document hosted at https://hub.nomi.ai/index.json
// (statically served via GitHub Pages) that enumerates every
// marketplace-tier plugin: id, latest version, capabilities,
// network allowlist, install size, bundle URL, sha256 of the bundle.
//
// The catalog itself is signed with the same root key used for
// per-bundle signatures (ADR 0002 §2 + ADR 0003), so a hostile
// hub.nomi.ai mirror cannot serve a fake index without the daemon
// rejecting it. Per-bundle signature verification still happens at
// install time — the catalog signature only attests "this is the
// list NomiHub published," not "these bundles are safe."
//
// Daemon usage:
//
//	client := hub.NewClient(verifier, http.DefaultClient)
//	cat, err := client.Fetch(ctx, "https://hub.nomi.ai/index.json")
//	for _, entry := range cat.Entries { ... }
//
// Cache persistence + the daily poll loop live in lifecycle-10; this
// package only knows how to fetch + verify + parse on demand.
package hub

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/signing"
)

// SchemaVersion pins the wire format. v1 is the only version today.
// Bumping breaks compatibility with older daemons; expect to be
// additive (new optional fields) rather than incompatible for the
// foreseeable future.
const SchemaVersion = 1

// Entry is one plugin's catalog row. Carries enough metadata for the
// install dialog to render (capabilities, allowlist, size, readme
// excerpt) without downloading the full bundle. The bundle itself is
// fetched on click via BundleURL and verified against SHA256.
type Entry struct {
	PluginID             string    `json:"plugin_id"`
	Name                 string    `json:"name"`
	LatestVersion        string    `json:"latest_version"`
	Author               string    `json:"author,omitempty"`
	Description          string    `json:"description,omitempty"`
	Capabilities         []string  `json:"capabilities"`
	NetworkAllowlist     []string  `json:"network_allowlist,omitempty"`
	InstallSizeBytes     int64     `json:"install_size_bytes"`
	SHA256               string    `json:"sha256"`
	BundleURL            string    `json:"bundle_url"`
	PublisherFingerprint string    `json:"publisher_fingerprint"`
	PublishedAt          time.Time `json:"published_at"`
	ReadmeExcerpt        string    `json:"readme_excerpt,omitempty"`
}

// Catalog is the unsigned payload — the actual list of entries the
// daemon shows in the marketplace browser. Wrapped in SignedCatalog
// for over-the-wire transport.
type Catalog struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Entries       []Entry   `json:"entries"`
}

// SignedCatalog is the on-the-wire shape. The Catalog field is held
// as a json.RawMessage so the bytes the publisher signed are the
// exact bytes the verifier hashes — re-marshaling would lose
// whitespace and break the signature, same trick the bundle package
// uses with RawManifest.
type SignedCatalog struct {
	Catalog   json.RawMessage `json:"catalog"`
	Signature []byte          `json:"signature"` // ed25519 over Catalog bytes, base64-encoded
}

// Sentinel errors so callers (the marketplace endpoint, the update
// poller in lifecycle-10) can branch on cause without string matching.
var (
	ErrCatalogFetchFailed    = errors.New("hub: fetch failed")
	ErrCatalogSignatureBad   = errors.New("hub: catalog signature does not verify")
	ErrCatalogParseFailed    = errors.New("hub: catalog JSON malformed")
	ErrCatalogVersionUnknown = errors.New("hub: catalog schema version not understood")
)

// maxCatalogBytes caps the size we'll consider. 16 MiB comfortably
// fits any reasonable plugin index — at ~1 KiB per entry that is room
// for ~16,000 plugins, which we'll never have. Real bound is mostly
// for DoS resistance.
const maxCatalogBytes = 16 * 1024 * 1024

// Client fetches + verifies catalogs. Verifier holds the same root
// pubkey used for bundle verification (the catalog and bundles share
// the trust anchor). httpClient is injectable so tests can stub the
// transport without spinning up a real server.
type Client struct {
	verifier *signing.Verifier
	rootKey  ed25519.PublicKey
	http     *http.Client
}

// NewClient constructs a Client with the given root pubkey. The
// signing.Verifier bound to the same key is used for the wrapping
// envelope check. http may be nil — defaults to a 30s-timeout client.
func NewClient(rootKey ed25519.PublicKey, http *http.Client) (*Client, error) {
	v, err := signing.NewVerifier(rootKey, nil)
	if err != nil {
		return nil, fmt.Errorf("hub: verifier: %w", err)
	}
	if http == nil {
		http = defaultHTTPClient()
	}
	return &Client{verifier: v, rootKey: rootKey, http: http}, nil
}

// Fetch downloads, verifies, and parses the catalog at url.
func (c *Client) Fetch(ctx context.Context, url string) (*Catalog, error) {
	body, err := c.download(ctx, url)
	if err != nil {
		return nil, err
	}
	return c.Parse(body)
}

// Parse validates raw bytes (signature + schema) and returns the
// catalog. Split out from Fetch so the cache layer added in
// lifecycle-10 can rehydrate from disk without re-fetching.
func (c *Client) Parse(raw []byte) (*Catalog, error) {
	var signed SignedCatalog
	if err := json.Unmarshal(raw, &signed); err != nil {
		return nil, fmt.Errorf("%w: envelope: %v", ErrCatalogParseFailed, err)
	}
	if len(signed.Signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature is %d bytes, want %d",
			ErrCatalogSignatureBad, len(signed.Signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(c.rootKey, signed.Catalog, signed.Signature) {
		return nil, ErrCatalogSignatureBad
	}

	var cat Catalog
	if err := json.Unmarshal(signed.Catalog, &cat); err != nil {
		return nil, fmt.Errorf("%w: catalog: %v", ErrCatalogParseFailed, err)
	}
	if cat.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: got %d, daemon understands %d",
			ErrCatalogVersionUnknown, cat.SchemaVersion, SchemaVersion)
	}
	return &cat, nil
}

func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCatalogFetchFailed, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCatalogFetchFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d from %s", ErrCatalogFetchFailed, resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read: %v", ErrCatalogFetchFailed, err)
	}
	if len(body) > maxCatalogBytes {
		return nil, fmt.Errorf("%w: catalog exceeds %d bytes", ErrCatalogFetchFailed, maxCatalogBytes)
	}
	return body, nil
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// SignCatalog is the catalog-publisher helper: produces a SignedCatalog
// wrapping the canonical JSON bytes. Lives in this package — and
// takes the catalog as raw bytes, not as a Catalog struct — so the
// drift-prevention rule from the bundle/signing pair holds: the bytes
// signed must be exactly the bytes parsed.
func SignCatalog(rootKey ed25519.PrivateKey, catalogJSON []byte) ([]byte, error) {
	signature := ed25519.Sign(rootKey, catalogJSON)
	wrapped, err := json.Marshal(SignedCatalog{
		Catalog:   catalogJSON,
		Signature: signature,
	})
	if err != nil {
		return nil, err
	}
	return wrapped, nil
}
