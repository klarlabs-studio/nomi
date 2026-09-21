package mcpcatalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxCatalogBytes caps remote index size (DoS resistance). ~1 KiB/entry →
// room for thousands of presets.
const maxCatalogBytes = 8 * 1024 * 1024

// cacheTTL is how long a successful fetch is reused without re-download.
const cacheTTL = 6 * time.Hour

// fetchTimeout bounds a single catalog download.
const fetchTimeout = 20 * time.Second

// Client downloads, parses, and caches a remote MCP preset catalog.
type Client struct {
	http *http.Client
	mu   sync.RWMutex

	url       string
	presets   []Preset
	fetchedAt time.Time
	lastErr   string
}

// NewClient returns a Client. http may be nil (20s timeout default).
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: fetchTimeout}
	}
	return &Client{http: httpClient}
}

// Status is a snapshot for settings / GET responses.
type Status struct {
	URL         string    `json:"url"`
	PresetCount int       `json:"preset_count"`
	FetchedAt   time.Time `json:"fetched_at,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Stale       bool      `json:"stale"`
}

// SetURL updates the configured catalog URL and clears the cache.
// Empty URL disables remote presets.
func (c *Client) SetURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if err := ValidateCatalogURL(raw); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.url = raw
	c.presets = nil
	c.fetchedAt = time.Time{}
	c.lastErr = ""
	return nil
}

// URL returns the configured catalog URL (may be empty).
func (c *Client) URL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.url
}

// Status returns cache metadata without fetching.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	stale := false
	if c.url != "" && !c.fetchedAt.IsZero() {
		stale = time.Since(c.fetchedAt) > cacheTTL
	}
	return Status{
		URL:         c.url,
		PresetCount: len(c.presets),
		FetchedAt:   c.fetchedAt,
		LastError:   c.lastErr,
		Stale:       stale,
	}
}

// Presets returns cached remote presets, refreshing when missing/stale.
// When no URL is configured, returns nil (not an error).
func (c *Client) Presets(ctx context.Context) ([]Preset, error) {
	c.mu.RLock()
	url := c.url
	fresh := url != "" && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) <= cacheTTL && c.lastErr == ""
	cached := append([]Preset(nil), c.presets...)
	c.mu.RUnlock()

	if url == "" {
		return nil, nil
	}
	if fresh {
		return cached, nil
	}
	return c.Refresh(ctx)
}

// Refresh force-downloads the catalog. No-op (nil, nil) when URL empty.
func (c *Client) Refresh(ctx context.Context) ([]Preset, error) {
	c.mu.RLock()
	rawURL := c.url
	c.mu.RUnlock()
	if rawURL == "" {
		c.mu.Lock()
		c.presets = nil
		c.fetchedAt = time.Time{}
		c.lastErr = ""
		c.mu.Unlock()
		return nil, nil
	}

	presets, err := c.fetch(ctx, rawURL)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Ignore if URL changed mid-flight.
	if c.url != rawURL {
		return append([]Preset(nil), c.presets...), nil
	}
	if err != nil {
		c.lastErr = err.Error()
		// Keep previous successful cache if any.
		if len(c.presets) > 0 {
			return append([]Preset(nil), c.presets...), fmt.Errorf("%w (serving cached)", err)
		}
		return nil, err
	}
	c.presets = presets
	c.fetchedAt = time.Now().UTC()
	c.lastErr = ""
	return append([]Preset(nil), c.presets...), nil
}

func (c *Client) fetch(ctx context.Context, rawURL string) ([]Preset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpcatalog: request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.8")
	req.Header.Set("User-Agent", "nomid-mcp-catalog/1")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcpcatalog: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcpcatalog: fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mcpcatalog: read: %w", err)
	}
	if len(body) > maxCatalogBytes {
		return nil, fmt.Errorf("mcpcatalog: catalog exceeds %d bytes", maxCatalogBytes)
	}

	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	return Parse(body, host)
}

// ValidateCatalogURL requires http(s) with a host. http is allowed for
// localhost / private lab indexes; production catalogs should use https.
func ValidateCatalogURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("mcpcatalog: invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("mcpcatalog: URL scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("mcpcatalog: URL missing host")
	}
	return nil
}
