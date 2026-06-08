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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// generateTestKey returns a fresh 2048-bit RSA key + its PKCS1 PEM
// encoding. Tests exercising LoadAppCredentials want both the parsed
// key (for signature verification) and the PEM bytes (for the load
// path).
func generateTestKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, pemBytes
}

func TestLoadAppCredentials_PKCS1(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	creds, err := LoadAppCredentials(12345, pemBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if creds.AppID != 12345 {
		t.Fatalf("app id = %d, want 12345", creds.AppID)
	}
	if creds.PrivateKey == nil {
		t.Fatal("private key not loaded")
	}
}

func TestLoadAppCredentials_PKCS8(t *testing.T) {
	key, _ := generateTestKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	creds, err := LoadAppCredentials(99, pemBytes)
	if err != nil {
		t.Fatalf("load pkcs8: %v", err)
	}
	if creds.AppID != 99 {
		t.Fatalf("app id = %d", creds.AppID)
	}
}

func TestLoadAppCredentials_RejectsBadInputs(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	cases := []struct {
		name    string
		appID   int64
		pem     []byte
		wantErr string
	}{
		{"empty pem", 1, []byte("not a pem"), "no PEM block"},
		{"zero app id", 0, pemBytes, "must be positive"},
		{"unknown block type", 1, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{0}}), "unsupported PEM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadAppCredentials(tc.appID, tc.pem)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestMintAppJWT_Roundtrip(t *testing.T) {
	key, pemBytes := generateTestKey(t)
	creds, err := LoadAppCredentials(42, pemBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	jwt, err := creds.MintAppJWT(now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt should have 3 parts, got %d", len(parts))
	}

	// Verify the signature against the public key.
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("verify sig: %v", err)
	}

	// Decode + sanity-check claims.
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims["iss"] != "42" {
		t.Fatalf("iss = %v, want 42", claims["iss"])
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if int64(iat) >= int64(exp) {
		t.Fatalf("iat %v should be < exp %v", iat, exp)
	}
	if int64(exp)-int64(iat) > int64(jwtMaxLifetime/time.Second) {
		t.Fatalf("jwt lifetime exceeds GitHub's 10m cap")
	}
}

func TestInstallationToken_FetchAndCache(t *testing.T) {
	key, pemBytes := generateTestKey(t)
	creds, err := LoadAppCredentials(42, pemBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Sanity: verify the bearer JWT header was set.
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("missing bearer header: %q", auth)
		}
		// Verify the JWT signature against the App's public key.
		token := strings.TrimPrefix(auth, "Bearer ")
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Errorf("bad jwt: %q", token)
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
			t.Errorf("jwt sig invalid: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{
			"token": "ghs_fake_install_token",
			"expires_at": %q,
			"permissions": {"issues": "write", "pull_requests": "write"},
			"repositories": [{"id": 100}, {"id": 200}]
		}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := NewAuthClient(creds, WithAPIBase(srv.URL), WithHTTPClient(srv.Client()), WithClock(clock))

	tok, err := c.InstallationToken(context.Background(), 555)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if tok.Token != "ghs_fake_install_token" {
		t.Fatalf("token = %q", tok.Token)
	}
	if len(tok.RepoIDs) != 2 || tok.RepoIDs[0] != 100 {
		t.Fatalf("repo ids = %v", tok.RepoIDs)
	}

	// Second call within validity window must hit the cache.
	tok2, err := c.InstallationToken(context.Background(), 555)
	if err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if tok2 != tok {
		t.Fatalf("expected cached pointer, got fresh value")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 round-trip, got %d", calls.Load())
	}

	// Different installation forces a fresh mint.
	if _, err := c.InstallationToken(context.Background(), 666); err != nil {
		t.Fatalf("second installation: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 round-trips, got %d", calls.Load())
	}
}

func TestInstallationToken_Refresh_AfterExpiry(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	creds, err := LoadAppCredentials(42, pemBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// Each call returns a token that expires in 30s — well inside
		// the tokenRefreshSlack window so the next call must re-mint.
		_, _ = fmt.Fprintf(w, `{
			"token": "ghs_short_lived_%d",
			"expires_at": %q,
			"permissions": {}
		}`, calls.Load(), time.Now().Add(30*time.Second).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()

	c := NewAuthClient(creds, WithAPIBase(srv.URL), WithHTTPClient(srv.Client()))

	if _, err := c.InstallationToken(context.Background(), 1); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.InstallationToken(context.Background(), 1); err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected re-mint when token within refresh slack, got %d calls", calls.Load())
	}
}

func TestInstallationToken_HTTPError(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	creds, err := LoadAppCredentials(42, pemBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
	}))
	defer srv.Close()
	c := NewAuthClient(creds, WithAPIBase(srv.URL), WithHTTPClient(srv.Client()))
	_, err = c.InstallationToken(context.Background(), 1)
	if err == nil {
		t.Fatal("want error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err should reference status code: %v", err)
	}
}

func TestInvalidateInstallation_ForcesRefresh(t *testing.T) {
	_, pemBytes := generateTestKey(t)
	creds, err := LoadAppCredentials(42, pemBytes)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{
			"token": "t%d",
			"expires_at": %q
		}`, calls.Load(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	c := NewAuthClient(creds, WithAPIBase(srv.URL), WithHTTPClient(srv.Client()))
	if _, err := c.InstallationToken(context.Background(), 1); err != nil {
		t.Fatalf("first: %v", err)
	}
	c.InvalidateInstallation(1)
	if _, err := c.InstallationToken(context.Background(), 1); err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("invalidate should force re-mint, got %d calls", calls.Load())
	}
}
