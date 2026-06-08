package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// TokenStore holds the active bearer token in memory + on disk and lets a
// rotation handler swap it atomically. The middleware reads from this on
// every request so a rotation takes effect immediately for all subsequent
// calls; in-flight requests authenticated under the old token finish but
// can't follow up with a new request.
type TokenStore struct {
	current atomic.Value // string
	path    string
}

// NewTokenStore wraps an existing token (typically the one loaded at
// startup) with the file path it was persisted to. Rotation rewrites the
// file in place; readers always see the latest value.
func NewTokenStore(initial, path string) *TokenStore {
	s := &TokenStore{path: path}
	s.current.Store(initial)
	return s
}

// Current returns the active token. Constant-time comparison is the
// caller's responsibility (RequireAuthToken handles it).
func (s *TokenStore) Current() string {
	if v, ok := s.current.Load().(string); ok {
		return v
	}
	return ""
}

// Rotate generates a fresh token, writes it to disk with 0600 perms, and
// makes it the active value for subsequent requests. Returns the new
// token so the caller (typically the /auth/rotate handler) can hand it
// back to the user once.
func (s *TokenStore) Rotate() (string, error) {
	buf := make([]byte, tokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(s.path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("failed to write token file: %w", err)
	}
	s.current.Store(token)
	return token, nil
}

// AuthTokenFilename is the name of the bearer-token file inside the app data directory.
const AuthTokenFilename = "auth.token"

// APIEndpointFilename is the name of the endpoint-discovery file. Clients
// (the Tauri shell, e2e harness) read this to learn the URL nomid is bound
// to instead of hardcoding a port. Written atomically alongside the auth
// token at startup so the two are always in sync.
const APIEndpointFilename = "api.endpoint"

// APIEndpoint is the JSON shape persisted in api.endpoint. Kept minimal so
// the Tauri side can deserialize it without pulling in extra fields.
type APIEndpoint struct {
	URL  string `json:"url"`
	Port string `json:"port"`
}

// WriteAPIEndpoint persists the URL nomid is bound to so non-Go clients can
// discover it. Mode 0600 because the URL is paired with the bearer token
// and shares its trust boundary (same user account that started nomid).
func WriteAPIEndpoint(dataDir, url, port string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create data directory: %w", err)
	}
	path := filepath.Join(dataDir, APIEndpointFilename)
	payload, err := json.Marshal(APIEndpoint{URL: url, Port: port})
	if err != nil {
		return "", fmt.Errorf("failed to marshal endpoint: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", fmt.Errorf("failed to write endpoint file: %w", err)
	}
	return path, nil
}

// tokenByteLength is the raw entropy size before hex encoding.
const tokenByteLength = 32

// LoadOrGenerateAuthToken loads the API bearer token from the given data directory,
// generating and persisting a new token on first run. The token file is written with
// 0600 permissions; if an existing file is unreadable, shorter than expected, or
// empty, a fresh token is generated.
//
// Returns the token value and the absolute path of the token file.
func LoadOrGenerateAuthToken(dataDir string) (string, string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", "", fmt.Errorf("failed to create data directory: %w", err)
	}
	path := filepath.Join(dataDir, AuthTokenFilename)

	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) >= tokenByteLength*2 {
			return token, path, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("failed to read token file %s: %w", path, err)
	}

	buf := make([]byte, tokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("failed to generate token: %w", err)
	}
	token := hex.EncodeToString(buf)

	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", "", fmt.Errorf("failed to write token file: %w", err)
	}
	return token, path, nil
}

// publicPaths are the URL paths that do not require a bearer token. /health is
// exposed so clients can probe reachability before they have read the token;
// /version is exposed so the auto-updater and About panel can read build info
// before the token is available.
var publicPaths = map[string]bool{
	"/health":  true,
	"/version": true,
	// /metrics is the Prometheus scrape endpoint. Public so a scraper
	// (which doesn't carry a bearer token) can pull it; operators with
	// stricter requirements should restrict access at the reverse-proxy
	// or firewall layer.
	"/metrics": true,
}

// RequireAuthToken returns middleware that rejects any request without a
// matching bearer token. CORS preflights (OPTIONS) and entries in
// publicPaths bypass the check. Comparison is constant-time.
//
// Accepts either a static string (boot-time token, used by tests) or a
// *TokenStore for runtime rotation. The store path is what production
// uses: /auth/rotate updates the store, the next request reads the new
// value, the old token stops working.
func RequireAuthToken(expected interface{}) gin.HandlerFunc {
	resolve := func() string { return "" }
	switch v := expected.(type) {
	case string:
		resolve = func() string { return v }
	case *TokenStore:
		if v == nil {
			panic("RequireAuthToken: nil TokenStore")
		}
		resolve = v.Current
	default:
		panic(fmt.Sprintf("RequireAuthToken: unsupported expected type %T", expected))
	}

	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if publicPaths[c.Request.URL.Path] {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/webhooks/") {
			c.Next()
			return
		}

		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		presented := []byte(strings.TrimPrefix(header, prefix))
		expectedBytes := []byte(resolve())
		if len(expectedBytes) == 0 || subtle.ConstantTimeCompare(presented, expectedBytes) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid bearer token"})
			return
		}
		c.Next()
	}
}

// allowedOrigins enumerates the origins Nomi accepts CORS from. These cover Tauri
// production (tauri://localhost and the Windows http variant), the Vite dev server,
// and the Playwright preview server.
var allowedOrigins = map[string]bool{
	"tauri://localhost":       true,
	"http://tauri.localhost":  true,
	"https://tauri.localhost": true,
	"http://localhost:1420":   true,
	"http://localhost:5173":   true,
	"http://localhost:4173":   true,
	"http://127.0.0.1:5173":   true,
	"http://127.0.0.1:4173":   true,
}

// CORSMiddleware returns a strict CORS handler. Origins are allowlisted and the
// response echoes the request origin back rather than sending a wildcard; this is
// required because requests carry a bearer token.
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && allowedOrigins[origin] {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Vary", "Origin")
			// PATCH must be explicitly listed — browsers preflight any
			// non-simple method and will block the request if the
			// server doesn't echo it back here. /plugins/:id/state and
			// other partial-update endpoints rely on PATCH.
			c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			c.Writer.Header().Set("Access-Control-Max-Age", "600")
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
