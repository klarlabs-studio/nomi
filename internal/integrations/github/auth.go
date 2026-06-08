// Package github implements the GitHub App auth chain that the
// GitHub plugin uses for per-Connection installation tokens. PAT
// authentication is intentionally NOT supported here — the spec calls
// for first-class GitHub App attribution (bot avatar on PRs, scoped
// repo access, revocable installation) which a fine-grained PAT can't
// provide. The plugin layer (github-02) maps PluginConnection rows to
// installation IDs on this package.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	defaultAPIBase    = "https://api.github.com"
	jwtMaxLifetime    = 10 * time.Minute // GitHub caps app JWTs at 10m
	jwtSafetyMargin   = 30 * time.Second // mint with headroom so clock skew doesn't reject
	tokenRefreshSlack = 60 * time.Second // refresh installation tokens this long before expiry
)

// AppCredentials identifies a GitHub App registration. The App ID is
// public; the private key is the RSA key generated when the App was
// created and is the only secret needed to mint JWTs.
type AppCredentials struct {
	AppID      int64
	PrivateKey *rsa.PrivateKey
}

// LoadAppCredentials parses an App ID + PEM-encoded RSA private key.
// The PEM may be either PKCS#1 ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE
// KEY") — GitHub gives users PKCS#1 by default but downstream tooling
// often re-encodes as PKCS#8.
func LoadAppCredentials(appID int64, privateKeyPEM []byte) (*AppCredentials, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("github: no PEM block in private key")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github: parse PKCS1 key: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github: parse PKCS8 key: %w", err)
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github: PKCS8 key is not RSA")
		}
		key = rsaKey
	default:
		return nil, fmt.Errorf("github: unsupported PEM block type %q", block.Type)
	}
	if appID <= 0 {
		return nil, errors.New("github: app id must be positive")
	}
	return &AppCredentials{AppID: appID, PrivateKey: key}, nil
}

// MintAppJWT produces a short-lived RS256 JWT identifying the App. The
// JWT is the bearer credential for endpoints under /app, /app/installations/...,
// and the installation-token-exchange flow.
func (c *AppCredentials) MintAppJWT(now time.Time) (string, error) {
	if c == nil || c.PrivateKey == nil {
		return "", errors.New("github: nil credentials")
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		// iat backdated by safety margin guards against minor clock
		// skew between this host and GitHub's issuance check.
		"iat": now.Add(-jwtSafetyMargin).Unix(),
		"exp": now.Add(jwtMaxLifetime - jwtSafetyMargin).Unix(),
		"iss": strconv.FormatInt(c.AppID, 10),
	}
	headerB, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsB, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerB) + "." + enc.EncodeToString(claimsB)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign JWT: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

// InstallationToken is the short-lived (≤1h) bearer credential for
// API calls scoped to a single installation. Permissions and
// repo allowlist are decided server-side based on the App's
// configuration + the installer's choices.
type InstallationToken struct {
	Token       string
	ExpiresAt   time.Time
	Permissions map[string]string
	RepoIDs     []int64 // empty when the installation grants all repos
}

// Valid reports whether the token can still be used for at least one
// more API call without an imminent refresh.
func (t *InstallationToken) Valid(now time.Time) bool {
	if t == nil || t.Token == "" {
		return false
	}
	return now.Add(tokenRefreshSlack).Before(t.ExpiresAt)
}

// AuthClient mints + caches installation tokens from app-level
// credentials. One AuthClient per AppCredentials; multiple
// installation IDs share the same client.
type AuthClient struct {
	creds   *AppCredentials
	http    *http.Client
	apiBase string
	now     func() time.Time

	mu     sync.Mutex
	tokens map[int64]*InstallationToken // installationID → token
}

// NewAuthClient wraps app credentials with the HTTP machinery needed
// to mint installation tokens. Callers reuse a single AuthClient
// across goroutines.
func NewAuthClient(creds *AppCredentials, opts ...AuthClientOption) *AuthClient {
	c := &AuthClient{
		creds:   creds,
		http:    &http.Client{Timeout: 30 * time.Second},
		apiBase: defaultAPIBase,
		now:     time.Now,
		tokens:  make(map[int64]*InstallationToken),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// AuthClientOption tweaks an AuthClient. Tests use these to swap the
// HTTP client, API base URL, and clock without modifying production
// constructors.
type AuthClientOption func(*AuthClient)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(h *http.Client) AuthClientOption { return func(c *AuthClient) { c.http = h } }

// WithAPIBase overrides https://api.github.com (used by tests + GHES).
func WithAPIBase(base string) AuthClientOption { return func(c *AuthClient) { c.apiBase = base } }

// WithClock overrides time.Now (used by tests).
func WithClock(now func() time.Time) AuthClientOption { return func(c *AuthClient) { c.now = now } }

// InstallationToken returns a valid token for the given installation,
// minting + caching one if necessary. Concurrent callers for the same
// installation share a single mint via the per-client mutex; collapsing
// stampedes on token expiry avoids smacking GitHub's app-level rate
// limit.
func (c *AuthClient) InstallationToken(ctx context.Context, installationID int64) (*InstallationToken, error) {
	c.mu.Lock()
	tok, ok := c.tokens[installationID]
	if ok && tok.Valid(c.now()) {
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	// Mint outside the lock so other installations aren't blocked while
	// we're round-tripping to GitHub. Re-check under the lock before
	// publishing in case another goroutine raced us and won.
	minted, err := c.mintInstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.tokens[installationID]; ok && existing.Valid(c.now()) {
		return existing, nil
	}
	c.tokens[installationID] = minted
	return minted, nil
}

func (c *AuthClient) mintInstallationToken(ctx context.Context, installationID int64) (*InstallationToken, error) {
	jwt, err := c.creds.MintAppJWT(c.now())
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.apiBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: installation token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("github: installation token HTTP %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("github: decode installation token: %w", err)
	}

	repoIDs := make([]int64, 0, len(payload.Repositories))
	for _, r := range payload.Repositories {
		repoIDs = append(repoIDs, r.ID)
	}
	return &InstallationToken{
		Token:       payload.Token,
		ExpiresAt:   payload.ExpiresAt,
		Permissions: payload.Permissions,
		RepoIDs:     repoIDs,
	}, nil
}

// InvalidateInstallation drops any cached token for the given
// installation. Useful after a 401 from a downstream call so the next
// request mints fresh rather than retrying with a known-bad token.
func (c *AuthClient) InvalidateInstallation(installationID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, installationID)
}
