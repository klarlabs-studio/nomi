package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/secrets"
)

type memStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemStore() *memStore { return &memStore{data: map[string]string{}} }

func (s *memStore) Put(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[k] = v
	return nil
}
func (s *memStore) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (s *memStore) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, k)
	return nil
}

// newTestManager wires the OAuthManager against an httptest server. The two
// Google endpoints (/device/code and /token) are the only URLs we need to
// intercept, so we override them via unexported package vars through a small
// helper instead of plumbing a base URL through every call site.
func newTestManager(t *testing.T, deviceURL, tokenURL string, store secrets.Store) *OAuthManager {
	t.Helper()
	m := NewOAuthManager(store)
	m.deviceCodeEndpoint = deviceURL
	m.tokenEndpoint = tokenURL
	return m
}

func TestStartDeviceFlow_StoresSessionAndReturnsUserCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("client_id") != "test-client" {
			t.Fatalf("unexpected client_id: %s", r.Form.Get("client_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-123",
			"user_code":        "ABCD-EFGH",
			"verification_url": "https://www.google.com/device",
			"expires_in":       1800,
			"interval":         5,
		})
	}))
	defer srv.Close()

	m := newTestManager(t, srv.URL, "unused", nil)
	sess, err := m.StartDeviceFlow(context.Background(), "test-client", "acct-1")
	if err != nil {
		t.Fatalf("StartDeviceFlow: %v", err)
	}
	if sess.UserCode != "ABCD-EFGH" {
		t.Fatalf("user code: %s", sess.UserCode)
	}
	if sess.VerificationURL != "https://www.google.com/device" {
		t.Fatalf("verification url: %s", sess.VerificationURL)
	}
	if sess.AccountID != "acct-1" {
		t.Fatalf("account id: %s", sess.AccountID)
	}

	got, ok := m.GetSession("dev-123")
	if !ok {
		t.Fatal("expected session to be stored by device_code")
	}
	if got.UserCode != "ABCD-EFGH" {
		t.Fatalf("stored session user code: %s", got.UserCode)
	}
}

func TestPollToken_SuccessStoresRefreshToken(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-xyz",
			"refresh_token": "rt-xyz",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	store := newMemStore()
	m := newTestManager(t, "unused", srv.URL, store)
	// Seed a session manually so we don't also need a device-code server.
	sess := &DeviceSession{
		DeviceCode: "dev-abc",
		AccountID:  "acct-1",
		ClientID:   "test-client",
		Interval:   1, // clamp kicks this up to 5s; we override below
		ExpiresAt:  time.Now().Add(5 * time.Minute),
	}
	m.sessions["dev-abc"] = sess
	// Drop the poll interval to something tests can actually wait for.
	m.minInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := m.PollToken(ctx, "dev-abc")
	if err != nil {
		t.Fatalf("PollToken: %v", err)
	}
	if result.AccessToken != "at-xyz" {
		t.Fatalf("access token: %s", result.AccessToken)
	}
	if result.RefreshToken != "rt-xyz" {
		t.Fatalf("refresh token: %s", result.RefreshToken)
	}

	stored, err := store.Get(refreshTokenKey("acct-1"))
	if err != nil {
		t.Fatalf("refresh token not stored: %v", err)
	}
	if stored != "rt-xyz" {
		t.Fatalf("stored refresh token mismatch: %s", stored)
	}
}

func TestPollToken_AccessDeniedReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	}))
	defer srv.Close()

	store := newMemStore()
	m := newTestManager(t, "unused", srv.URL, store)
	m.sessions["dev-d"] = &DeviceSession{
		DeviceCode: "dev-d",
		AccountID:  "acct-1",
		ClientID:   "test-client",
		Interval:   1,
		ExpiresAt:  time.Now().Add(5 * time.Minute),
	}
	m.minInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := m.PollToken(ctx, "dev-d"); err == nil {
		t.Fatal("expected access_denied error")
	}

	if _, err := store.Get(refreshTokenKey("acct-1")); err == nil {
		t.Fatal("refresh token should not be stored on denial")
	}
}

func TestGetToken_RefreshesUsingStoredRefreshToken(t *testing.T) {
	var seenGrant string
	var seenRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		seenGrant = r.Form.Get("grant_type")
		seenRefresh = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-refreshed",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        "gmail.modify calendar",
		})
	}))
	defer srv.Close()

	store := newMemStore()
	_ = store.Put(refreshTokenKey("acct-1"), "rt-stored")

	m := newTestManager(t, "unused", srv.URL, store)
	tok, err := m.GetToken(context.Background(), "acct-1", "test-client")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if tok.AccessToken != "at-refreshed" {
		t.Fatalf("access token: %s", tok.AccessToken)
	}
	if seenGrant != "refresh_token" {
		t.Fatalf("grant_type: %s", seenGrant)
	}
	if seenRefresh != "rt-stored" {
		t.Fatalf("refresh_token: %s", seenRefresh)
	}
}

func TestRevokeAccount_DeletesStoredRefreshToken(t *testing.T) {
	store := newMemStore()
	_ = store.Put(refreshTokenKey("acct-1"), "rt-stored")

	m := newTestManager(t, "unused", "unused", store)
	if err := m.RevokeAccount("acct-1"); err != nil {
		t.Fatalf("RevokeAccount: %v", err)
	}
	if _, err := store.Get(refreshTokenKey("acct-1")); err == nil {
		t.Fatal("refresh token should be deleted")
	}
}
