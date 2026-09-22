package hub

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SettingKey is the app_settings key for the marketplace catalog URL.
const SettingKey = "marketplace_catalog_url"

// DefaultCatalogURL is the production NomiHub index. Empty catalogs /
// unreachable hosts degrade browse to an error the UI surfaces; the
// install path still works from a direct bundle URL.
const DefaultCatalogURL = "https://hub.nomi.ai/index.json"

// cacheTTL is how long a successful fetch is reused without re-download.
const cacheTTL = 1 * time.Hour

// CachedProvider is the daemon-side catalog source: fetch-on-demand
// with an in-process TTL cache, plus SetURL / Refresh so Settings can
// point at a private or example catalog without restarting nomid.
type CachedProvider struct {
	client *Client
	mu     sync.Mutex
	url    string
	cached *Catalog
	at     time.Time
	err    string
}

// NewCachedProvider wraps a hub Client. url may be empty (defaults to
// DefaultCatalogURL on first Fetch).
func NewCachedProvider(client *Client, catalogURL string) *CachedProvider {
	return &CachedProvider{
		client: client,
		url:    strings.TrimSpace(catalogURL),
	}
}

// URL returns the configured catalog URL (empty means default).
func (p *CachedProvider) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

// EffectiveURL is the URL Fetch will hit.
func (p *CachedProvider) EffectiveURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.url == "" {
		return DefaultCatalogURL
	}
	return p.url
}

// Status is a snapshot for settings responses.
type Status struct {
	URL          string    `json:"url"`
	EffectiveURL string    `json:"effective_url"`
	EntryCount   int       `json:"entry_count"`
	FetchedAt    time.Time `json:"fetched_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Stale        bool      `json:"stale"`
}

// Status returns cache metadata without fetching.
func (p *CachedProvider) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	stale := false
	if !p.at.IsZero() {
		stale = time.Since(p.at) > cacheTTL
	}
	count := 0
	if p.cached != nil {
		count = len(p.cached.Entries)
	}
	eff := p.url
	if eff == "" {
		eff = DefaultCatalogURL
	}
	return Status{
		URL:          p.url,
		EffectiveURL: eff,
		EntryCount:   count,
		FetchedAt:    p.at,
		LastError:    p.err,
		Stale:        stale,
	}
}

// SetURL updates the catalog URL and clears the cache. Empty resets
// to DefaultCatalogURL on next fetch.
func (p *CachedProvider) SetURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if err := ValidateCatalogURL(raw); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.url = raw
	p.cached = nil
	p.at = time.Time{}
	p.err = ""
	return nil
}

// Get returns the cached catalog, refreshing when missing or stale.
func (p *CachedProvider) Get(ctx context.Context) (*Catalog, error) {
	p.mu.Lock()
	fresh := p.cached != nil && time.Since(p.at) <= cacheTTL && p.err == ""
	if fresh {
		cat := p.cached
		p.mu.Unlock()
		return cat, nil
	}
	p.mu.Unlock()
	return p.Refresh(ctx)
}

// Refresh force-downloads the catalog.
func (p *CachedProvider) Refresh(ctx context.Context) (*Catalog, error) {
	u := p.EffectiveURL()
	cat, err := p.client.Fetch(ctx, u)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.err = err.Error()
		if p.cached != nil {
			return p.cached, fmt.Errorf("%w (serving cached)", err)
		}
		return nil, err
	}
	p.cached = cat
	p.at = time.Now().UTC()
	p.err = ""
	return cat, nil
}

// ProviderFunc adapts CachedProvider to the CatalogProvider func shape
// used by the API router.
func (p *CachedProvider) ProviderFunc() func(ctx context.Context) (*Catalog, error) {
	return p.Get
}

// ValidateCatalogURL requires http(s) with a host.
func ValidateCatalogURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("hub: invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("hub: URL scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("hub: URL missing host")
	}
	return nil
}
