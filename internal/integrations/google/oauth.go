// Package google implements Google Workspace connectors (Gmail, Calendar)
// using OAuth 2.0 device flow for authentication.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/secrets"
)

const (
	deviceCodeURL = "https://oauth2.googleapis.com/device/code"
	tokenURL      = "https://oauth2.googleapis.com/token"
	gmailScope    = "https://www.googleapis.com/auth/gmail.modify"
	calendarScope = "https://www.googleapis.com/auth/calendar"
)

// OAuthManager handles Google OAuth 2.0 device flow for TV and limited-input
// device applications. It stores refresh tokens in the provided secrets.Store.
type OAuthManager struct {
	secrets secrets.Store
	client  *http.Client
	scopes  []string

	// Endpoint overrides and minimum poll interval are exposed for tests
	// so we can swap in an httptest server without adding a base-URL
	// argument to every call site. Defaults match Google's production URLs.
	deviceCodeEndpoint string
	tokenEndpoint      string
	minInterval        time.Duration

	mu       sync.RWMutex
	sessions map[string]*DeviceSession // keyed by device_code
}

// DeviceSession tracks an in-progress device authorization.
type DeviceSession struct {
	DeviceCode      string    `json:"device_code"`
	UserCode        string    `json:"user_code"`
	VerificationURL string    `json:"verification_url"`
	ExpiresAt       time.Time `json:"expires_at"`
	Interval        int       `json:"interval"`
	AccountID       string    `json:"account_id"` // caller-supplied ID
	ClientID        string    `json:"client_id"`
	Scopes          []string  `json:"scopes"`

	// Result fields set on success
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	ExpiresIn    int       `json:"expires_in,omitempty"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// TokenSet holds the current access token and metadata for an account.
type TokenSet struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresAt   time.Time `json:"expires_at"`
	Scope       string    `json:"scope"`
}

// NewOAuthManager creates a new OAuth manager with the given secret store.
func NewOAuthManager(secretStore secrets.Store) *OAuthManager {
	return &OAuthManager{
		secrets:            secretStore,
		client:             &http.Client{Timeout: 30 * time.Second},
		sessions:           make(map[string]*DeviceSession),
		scopes:             []string{gmailScope, calendarScope},
		deviceCodeEndpoint: deviceCodeURL,
		tokenEndpoint:      tokenURL,
		minInterval:        5 * time.Second,
	}
}

// SetScopes overrides the default scopes. Must be called before StartDeviceFlow.
func (m *OAuthManager) SetScopes(scopes []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scopes = scopes
}

// StartDeviceFlow initiates the OAuth device flow for a Google account.
// It returns a DeviceSession containing the user_code and verification_url
// that the user must visit to authorize the application.
func (m *OAuthManager) StartDeviceFlow(ctx context.Context, clientID, accountID string) (*DeviceSession, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("scope", strings.Join(m.scopes, " "))

	req, err := http.NewRequestWithContext(ctx, "POST", m.deviceCodeEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code request returned %d", resp.StatusCode)
	}

	var result struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode device code response: %w", err)
	}

	session := &DeviceSession{
		DeviceCode:      result.DeviceCode,
		UserCode:        result.UserCode,
		VerificationURL: result.VerificationURL,
		ExpiresAt:       time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
		Interval:        result.Interval,
		AccountID:       accountID,
		ClientID:        clientID,
		Scopes:          m.scopes,
	}

	m.mu.Lock()
	m.sessions[result.DeviceCode] = session
	m.mu.Unlock()

	return session, nil
}

// PollToken polls the OAuth token endpoint for the given device_code until
// the user completes authorization, the code expires, or the context is cancelled.
// On success, the refresh token is stored in the secrets store and the session
// is updated with the access token.
func (m *OAuthManager) PollToken(ctx context.Context, deviceCode string) (*DeviceSession, error) {
	m.mu.RLock()
	session, ok := m.sessions[deviceCode]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown device code")
	}

	interval := time.Duration(session.Interval) * time.Second
	if interval < m.minInterval {
		interval = m.minInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(session.ExpiresAt) {
				return nil, fmt.Errorf("device code expired")
			}

			tokenResp, err := m.exchangeDeviceCode(ctx, session)
			if err != nil {
				continue // polling error, retry
			}

			if tokenResp.Error != "" {
				switch tokenResp.Error {
				case "authorization_pending":
					continue
				case "slow_down":
					interval += 5 * time.Second
					ticker.Reset(interval)
					continue
				case "expired_token":
					return nil, fmt.Errorf("device code expired")
				case "access_denied":
					return nil, fmt.Errorf("user denied access")
				default:
					return nil, fmt.Errorf("oauth error: %s", tokenResp.Error)
				}
			}

			// Success
			session.AccessToken = tokenResp.AccessToken
			session.RefreshToken = tokenResp.RefreshToken
			session.TokenType = tokenResp.TokenType
			session.ExpiresIn = tokenResp.ExpiresIn
			session.CompletedAt = time.Now()

			// Store refresh token in secrets store
			if session.RefreshToken != "" && m.secrets != nil {
				key := refreshTokenKey(session.AccountID)
				if err := m.secrets.Put(key, session.RefreshToken); err != nil {
					return session, fmt.Errorf("token received but failed to store refresh token: %w", err)
				}
			}

			m.mu.Lock()
			m.sessions[deviceCode] = session
			m.mu.Unlock()

			return session, nil
		}
	}
}

func (m *OAuthManager) exchangeDeviceCode(ctx context.Context, session *DeviceSession) (*tokenResponse, error) {
	data := url.Values{}
	data.Set("client_id", session.ClientID)
	data.Set("device_code", session.DeviceCode)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, "POST", m.tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error,omitempty"`
}

// GetSession returns a session by device code.
func (m *OAuthManager) GetSession(deviceCode string) (*DeviceSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[deviceCode]
	if !ok {
		return nil, false
	}
	// Return a copy
	cp := *s
	return &cp, true
}

// GetToken retrieves a valid access token for the given account, refreshing
// it from the stored refresh token if expired or near expiry.
func (m *OAuthManager) GetToken(ctx context.Context, accountID, clientID string) (*TokenSet, error) {
	if m.secrets == nil {
		return nil, fmt.Errorf("no secret store configured")
	}

	key := refreshTokenKey(accountID)
	refreshToken, err := m.secrets.Get(key)
	if err != nil {
		return nil, fmt.Errorf("no refresh token found for account %s: %w", accountID, err)
	}

	return m.refreshToken(ctx, clientID, refreshToken)
}

// InvalidateAccount clears the stored refresh token for an account.
// Call this when receiving 401 Unauthorized responses to force re-auth.
func (m *OAuthManager) InvalidateAccount(accountID string) error {
	if m.secrets == nil {
		return nil
	}
	return m.secrets.Delete(refreshTokenKey(accountID))
}

// RefreshTokenForAccount refreshes the access token using the stored refresh token.
func (m *OAuthManager) RefreshTokenForAccount(ctx context.Context, accountID, clientID string) (*TokenSet, error) {
	return m.GetToken(ctx, accountID, clientID)
}

func (m *OAuthManager) refreshToken(ctx context.Context, clientID, refreshToken string) (*TokenSet, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, "POST", m.tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token request returned %d", resp.StatusCode)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode refresh response: %w", err)
	}

	return &TokenSet{
		AccessToken: result.AccessToken,
		TokenType:   result.TokenType,
		ExpiresAt:   time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
		Scope:       result.Scope,
	}, nil
}

// RevokeAccount removes the stored refresh token for an account.
func (m *OAuthManager) RevokeAccount(accountID string) error {
	if m.secrets == nil {
		return nil
	}
	return m.secrets.Delete(refreshTokenKey(accountID))
}

// CleanupSessions removes expired sessions.
func (m *OAuthManager) CleanupSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for code, s := range m.sessions {
		if now.After(s.ExpiresAt) && s.CompletedAt.IsZero() {
			delete(m.sessions, code)
		}
	}
}

func refreshTokenKey(accountID string) string {
	return "google/" + accountID + "/refresh_token"
}
