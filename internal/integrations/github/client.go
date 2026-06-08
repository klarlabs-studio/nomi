package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Client is a thin HTTP wrapper around an AuthClient that mints
// installation tokens on demand. Plugin tools use Client to talk to
// the GitHub API without re-implementing the token-cache + request-auth
// dance per call site.
type Client struct {
	auth           *AuthClient
	installationID int64
	apiBase        string
	http           *http.Client
}

// NewClient binds an installation ID to an AuthClient. One Client per
// (Connection, request) is fine — Clients are cheap; the underlying
// AuthClient owns the token cache.
func NewClient(auth *AuthClient, installationID int64, opts ...ClientOption) *Client {
	c := &Client{
		auth:           auth,
		installationID: installationID,
		apiBase:        auth.apiBase,
		http:           auth.http,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ClientOption tweaks a Client for tests (custom HTTP client + base
// URL when running against an httptest.Server).
type ClientOption func(*Client)

// WithClientAPIBase overrides the default api.github.com base URL.
func WithClientAPIBase(base string) ClientOption { return func(c *Client) { c.apiBase = base } }

// WithClientHTTP overrides the http.Client.
func WithClientHTTP(h *http.Client) ClientOption { return func(c *Client) { c.http = h } }

// RateLimitError is returned when the GitHub API surfaces an
// X-RateLimit-Remaining: 0 response. Callers can type-assert for it
// to back off rather than propagating a generic 4xx.
type RateLimitError struct {
	Remaining int
	ResetAt   time.Time
	Body      string
}

func (e *RateLimitError) Error() string {
	if !e.ResetAt.IsZero() {
		return fmt.Sprintf("github rate limit reached, resets at %s", e.ResetAt.UTC().Format(time.RFC3339))
	}
	return "github rate limit reached"
}

// HTTPError captures non-rate-limit non-2xx responses with the
// status code and decoded body so callers can render meaningful
// messages without rewrapping.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github HTTP %d: %s", e.StatusCode, e.Body)
}

// IsHTTPStatus reports whether err is an HTTPError with the given code.
// Tools use this to map upstream 403/404/422 to typed runtime errors.
func IsHTTPStatus(err error, code int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.StatusCode == code
}

// Do performs an authenticated HTTP request and decodes the JSON body
// into out (out=nil to discard). Returns RateLimitError or HTTPError
// on non-2xx responses.
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	tok, err := c.auth.InstallationToken(ctx, c.installationID)
	if err != nil {
		return fmt.Errorf("github: token: %w", err)
	}

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	url := c.apiBase + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 401 specifically: invalidate the cached token and bubble; the
	// next call mints fresh. Common when a token is revoked between
	// calls (e.g. installation removed).
	if resp.StatusCode == http.StatusUnauthorized {
		c.auth.InvalidateInstallation(c.installationID)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	// Rate limit detection. GitHub returns 403 when the limit is
	// exhausted, with X-RateLimit-Remaining: 0 and X-RateLimit-Reset
	// (unix seconds).
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resetUnix, _ := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		return &RateLimitError{
			Remaining: 0,
			ResetAt:   time.Unix(resetUnix, 0),
			Body:      string(bodyBytes),
		}
	}

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return &HTTPError{StatusCode: resp.StatusCode, Body: string(bodyBytes)}
}
