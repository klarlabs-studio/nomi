package webhooks

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Bot Framework OpenID metadata (channel service tokens).
const botFrameworkOpenID = "https://login.botframework.com/v1/.well-known/openidconfiguration"

// teamsAllowedIssuers are the JWT issuers Bot Framework uses.
var teamsAllowedIssuers = map[string]bool{
	"https://api.botframework.com":                                  true,
	"https://sts.windows.net/d6d49420-f39e-4b0a-8663-7f8e9a4d0b1f/": true,
}

// teamsKeyFunc is swapped in tests to avoid live JWKS fetches.
var teamsKeyFunc = defaultTeamsKeyFunc

type jwksCache struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	ttl     time.Duration
	client  *http.Client
}

var sharedJWKS = &jwksCache{
	keys:   map[string]*rsa.PublicKey{},
	ttl:    time.Hour,
	client: &http.Client{Timeout: 15 * time.Second},
}

type teamsVerifier struct{}

func (v *teamsVerifier) Verify(body []byte, appID string, headers map[string]string) error {
	_ = body // Bot Framework signs the Authorization JWT, not the body.
	if strings.TrimSpace(appID) == "" {
		return fmt.Errorf("missing microsoft app id")
	}
	auth := headerCI(headers, "Authorization")
	if auth == "" {
		return fmt.Errorf("missing Authorization header")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return fmt.Errorf("invalid Authorization scheme")
	}
	tokenStr := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	if tokenStr == "" {
		return fmt.Errorf("empty bearer token")
	}

	parser := jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}))
	tok, err := parser.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return teamsKeyFunc(t)
	})
	if err != nil {
		return fmt.Errorf("jwt parse: %w", err)
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok || !tok.Valid {
		return fmt.Errorf("invalid jwt claims")
	}
	iss, _ := claims.GetIssuer()
	if !teamsAllowedIssuers[iss] {
		// Also accept issuer that starts with sts.windows.net for multi-tenant.
		if !strings.HasPrefix(iss, "https://sts.windows.net/") {
			return fmt.Errorf("unexpected issuer %q", iss)
		}
	}
	aud, err := claims.GetAudience()
	if err != nil {
		return fmt.Errorf("missing audience")
	}
	audOK := false
	for _, a := range aud {
		if a == appID {
			audOK = true
			break
		}
	}
	if !audOK {
		return fmt.Errorf("audience mismatch")
	}
	return nil
}

func (v *teamsVerifier) EventType(headers map[string]string, body []byte) string {
	_ = headers
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Type == "" {
		return "teams_activity"
	}
	return "teams_" + envelope.Type
}

func defaultTeamsKeyFunc(t *jwt.Token) (interface{}, error) {
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("jwt missing kid")
	}
	return sharedJWKS.lookup(kid)
}

func (c *jwksCache) lookup(kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[kid]; ok && time.Since(c.fetched) < c.ttl {
		return key, nil
	}
	if err := c.refreshLocked(); err != nil {
		if key, ok := c.keys[kid]; ok {
			return key, nil
		}
		return nil, err
	}
	key, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("kid %q not in JWKS", kid)
	}
	return key, nil
}

func (c *jwksCache) refreshLocked() error {
	meta, err := c.getJSON(botFrameworkOpenID)
	if err != nil {
		return err
	}
	jwksURI, _ := meta["jwks_uri"].(string)
	if jwksURI == "" {
		return fmt.Errorf("openid metadata missing jwks_uri")
	}
	raw, err := c.getJSON(jwksURI)
	if err != nil {
		return err
	}
	keys, _ := raw["keys"].([]interface{})
	next := map[string]*rsa.PublicKey{}
	for _, item := range keys {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		kid, _ := m["kid"].(string)
		nStr, _ := m["n"].(string)
		eStr, _ := m["e"].(string)
		if kid == "" || nStr == "" || eStr == "" {
			continue
		}
		pub, err := rsaPublicFromJWK(nStr, eStr)
		if err != nil {
			continue
		}
		next[kid] = pub
	}
	if len(next) == 0 {
		return fmt.Errorf("empty JWKS")
	}
	c.keys = next
	c.fetched = time.Now()
	return nil
}

func (c *jwksCache) getJSON(url string) (map[string]interface{}, error) {
	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func rsaPublicFromJWK(nStr, eStr string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nb)
	var eInt int
	for _, b := range eb {
		eInt = eInt<<8 + int(b)
	}
	if eInt == 0 {
		return nil, fmt.Errorf("invalid exponent")
	}
	return &rsa.PublicKey{N: n, E: eInt}, nil
}
