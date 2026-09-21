package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/secrets"
)

// TokenURL is the Bot Framework client-credentials endpoint.
var TokenURL = "https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token"

type tokenCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

var connectorTokens sync.Map // appID → *tokenCache

type sendCreds struct {
	appID       string
	appPassword string
}

func (p *Plugin) resolveSendCreds(conn *domain.Connection) (sendCreds, error) {
	appIDRef, ok := conn.CredentialRefs["webhook_secret"]
	if !ok || appIDRef == "" {
		return sendCreds{}, fmt.Errorf("connection %s missing webhook_secret (Microsoft App ID)", conn.ID)
	}
	passRef, ok := conn.CredentialRefs["app_password"]
	if !ok || passRef == "" {
		return sendCreds{}, fmt.Errorf("connection %s missing app_password", conn.ID)
	}
	appID := appIDRef
	pass := passRef
	if p.secrets != nil {
		var err error
		appID, err = secrets.Resolve(p.secrets, appIDRef)
		if err != nil {
			return sendCreds{}, err
		}
		pass, err = secrets.Resolve(p.secrets, passRef)
		if err != nil {
			return sendCreds{}, err
		}
	}
	return sendCreds{appID: appID, appPassword: pass}, nil
}

func (p *Plugin) connectorToken(ctx context.Context, creds sendCreds) (string, error) {
	raw, _ := connectorTokens.LoadOrStore(creds.appID, &tokenCache{})
	cache := raw.(*tokenCache)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.token != "" && time.Now().Before(cache.expiresAt.Add(-30*time.Second)) {
		return cache.token, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", creds.appID)
	form.Set("client_secret", creds.appPassword)
	form.Set("scope", "https://api.botframework.com/.default")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("teams token: HTTP %d: %s", res.StatusCode, truncateRunes(string(body), 200))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("teams token: empty access_token")
	}
	exp := parsed.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	cache.token = parsed.AccessToken
	cache.expiresAt = time.Now().Add(time.Duration(exp) * time.Second)
	return cache.token, nil
}

func (p *Plugin) replyText(ctx context.Context, connID, serviceURL, conversationID, text string) error {
	conn, err := p.connections.GetByID(connID)
	if err != nil || conn == nil {
		return fmt.Errorf("teams reply: connection %s not found", connID)
	}
	creds, err := p.resolveSendCreds(conn)
	if err != nil {
		return err
	}
	token, err := p.connectorToken(ctx, creds)
	if err != nil {
		return err
	}
	return postActivity(ctx, token, serviceURL, conversationID, map[string]interface{}{
		"type": "message",
		"text": text,
	})
}

func (p *Plugin) replyAdaptiveCard(ctx context.Context, connID, serviceURL, conversationID, text string, card map[string]interface{}) error {
	conn, err := p.connections.GetByID(connID)
	if err != nil || conn == nil {
		return fmt.Errorf("teams card: connection %s not found", connID)
	}
	creds, err := p.resolveSendCreds(conn)
	if err != nil {
		return err
	}
	token, err := p.connectorToken(ctx, creds)
	if err != nil {
		return err
	}
	payload := map[string]interface{}{
		"type": "message",
		"text": text,
		"attachments": []map[string]interface{}{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content":     card,
		}},
	}
	return postActivity(ctx, token, serviceURL, conversationID, payload)
}

func postActivity(ctx context.Context, token, serviceURL, conversationID string, activity map[string]interface{}) error {
	serviceURL = strings.TrimRight(serviceURL, "/")
	if serviceURL == "" || conversationID == "" {
		return fmt.Errorf("teams post: serviceUrl and conversation id required")
	}
	buf, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/v3/conversations/%s/activities", serviceURL, url.PathEscape(conversationID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("teams post: HTTP %d: %s", res.StatusCode, truncateRunes(string(body), 200))
	}
	return nil
}
