// Package github implements the GitHub plugin — a tool-only plugin for
// reading and acting on GitHub issues, pull requests, and repositories
// via the GitHub App auth chain (see internal/integrations/github).
//
// This file is the scaffold: manifest, lifecycle hooks, no tools yet.
// Subsequent tasks (github-03..06) attach issues, pulls, repos, and
// polling triggers; github-07 wires the UI; github-08 ships the Code
// Reviewer assistant template that exercises the whole chain.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"go.klarlabs.de/nomi/internal/domain"
	gh "go.klarlabs.de/nomi/internal/integrations/github"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.github"

// Connection config keys. Stored in plugin_connections.config (JSON).
const (
	configAppID          = "app_id"          // int64 as JSON number
	configInstallationID = "installation_id" // int64 as JSON number
	configAccountLogin   = "account_login"   // org or user that installed the App
	configRepoAllowlist  = "repo_allowlist"  // []string of "owner/repo"
	configPollingEnabled = "polling_enabled" // bool, default false
	credentialPrivateKey = "private_key_pem" // ref into secrets.Store
)

// Plugin is the GitHub plugin. Tool-only; one Connection = one App
// installation. Polling triggers are added in github-06; until then
// the plugin only contributes tools that fire on explicit assistant
// requests.
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	secrets     secrets.Store

	// authOverride lets tests inject a fake AuthClient without going
	// through the secrets store. Production builds leave nil and
	// authClientFor builds one per Connection.
	authOverride func(conn *domain.Connection) (*gh.AuthClient, error)

	mu      sync.RWMutex
	running bool

	// authCache memoizes one AuthClient per (app_id, installation_id).
	// Tokens themselves cache inside AuthClient; the cache here just
	// avoids re-parsing the private key on every tool call.
	authCacheMu sync.Mutex
	authCache   map[string]*gh.AuthClient
}

// NewPlugin constructs the GitHub plugin scaffold.
func NewPlugin(
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		connections: conns,
		bindings:    binds,
		secrets:     secretStore,
		authCache:   make(map[string]*gh.AuthClient),
	}
}

// SetAuthOverride is for tests — replaces authClientFor with a
// caller-supplied factory so tools can run against a recorded mock
// without real GitHub App credentials.
func (p *Plugin) SetAuthOverride(fn func(conn *domain.Connection) (*gh.AuthClient, error)) {
	p.authOverride = fn
}

// Manifest declares the GitHub plugin's contract. Tools list grows in
// github-03..06; the scaffold ships an empty tool surface so the
// plugin registers cleanly without exposing call paths that don't
// exist yet.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "GitHub",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Read issues, review pull requests, and act on repositories via a GitHub App installation. One Connection per installed account or organization.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"github.read",
			"github.write",
			"network.outgoing",
			"filesystem.write", // github-05's clone tool writes into the workspace
		},
		Contributes: plugins.Contributions{
			Tools: p.toolContributions(),
			Triggers: []plugins.TriggerContribution{
				{Kind: TriggerKindIssueOpened, EventType: "github.issue_opened",
					Description: "Fire a run when a new issue appears in any allowlisted repo. Polls every 60s; the first poll establishes a baseline (no firings) so a daemon restart doesn't spam every existing open issue."},
				{Kind: TriggerKindPRReviewRequested, EventType: "github.pr_review_requested",
					Description: "Fire a run when a PR in an allowlisted repo gains a pending reviewer. First poll baselines current state."},
			},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{ //nolint:gosec // G101: credential field descriptor (label/description), not a secret value
					Kind:        "github_app_private_key",
					Key:         credentialPrivateKey,
					Label:       "GitHub App private key (PEM)",
					Required:    true,
					Description: "Paste the PKCS#1 or PKCS#8 PEM downloaded from the App's settings page when you generated the key. Begins with `-----BEGIN RSA PRIVATE KEY-----` or `-----BEGIN PRIVATE KEY-----`.",
				},
				{
					Kind:        "webhook_secret",
					Key:         "webhook_secret",
					Label:       "Webhook Secret",
					Required:    false,
					Description: "Secret used to verify webhook signatures from GitHub. Generate one in the Connections tab after enabling webhooks.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				configAppID: {
					Type: "string", Label: "GitHub App ID", Required: true,
					Description: "Numeric App ID from the GitHub App's settings page. Public, not secret.",
				},
				configInstallationID: {
					Type: "string", Label: "Installation ID", Required: true,
					Description: "Numeric installation ID returned when the App was installed. Find it in the URL after installing the App: github.com/settings/installations/<id>.",
				},
				configAccountLogin: {
					Type: "string", Label: "Account login", Required: true,
					Description: "GitHub login (org name or username) the installation belongs to. Used for connection-routing in multi-org setups.",
				},
				configRepoAllowlist: {
					Type: "string", Label: "Repository allowlist", Required: false,
					Description: "Comma-separated list of `owner/repo` the connection is permitted to access. Empty = whatever scope the installation grants. Required for polling triggers.",
				},
				configPollingEnabled: {
					Type: "boolean", Label: "Enable issue/PR polling", Required: false, Default: "false",
					Description: "When on, polls every 60s for new issues + PR review-requests on the allowlisted repos and emits trigger events to bound assistants.",
				},
			},
			NetworkAllowlist: []string{
				"api.github.com",
				"raw.githubusercontent.com",
				"codeload.github.com", // git clone over HTTPS hits this CDN
			},
		},
	}
}

// Configure is a no-op; per-Connection config lives on the Connection
// rows themselves rather than at the plugin level.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start marks the plugin running. The polling worker (github-06) hooks
// in here; for the scaffold there's nothing async to start.
func (p *Plugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	return nil
}

// Stop unwinds the running flag. The polling worker (github-06) will
// signal its goroutines to exit here.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

// Status reports plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// Tools implements plugins.ToolProvider. Aggregates every tool family
// the plugin supports — currently issues; pulls + repos arrive in
// github-04/05.
func (p *Plugin) Tools() []tools.Tool {
	defs := p.allToolDefs()
	out := make([]tools.Tool, 0, len(defs))
	for _, d := range defs {
		out = append(out, &pluginTool{plugin: p, def: d})
	}
	return out
}

// allToolDefs returns the union of every per-family tool list. Used
// by both Tools() (registry) and toolContributions() (manifest) so
// the two views stay in lock-step.
func (p *Plugin) allToolDefs() []toolDef {
	var all []toolDef
	all = append(all, p.issueTools()...)
	all = append(all, p.pullTools()...)
	all = append(all, p.repoTools()...)
	return all
}

// toolContributions renders the toolDef list into the manifest's
// ToolContribution shape.
func (p *Plugin) toolContributions() []plugins.ToolContribution {
	defs := p.allToolDefs()
	out := make([]plugins.ToolContribution, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.asContribution())
	}
	return out
}

// authClientFor resolves a GitHub AuthClient for the given Connection,
// caching across calls to avoid re-parsing the PEM private key. The
// cache key includes the App ID + installation ID so a single
// AuthClient is reused across tool invocations against the same
// installation.
func (p *Plugin) authClientFor(conn *domain.Connection) (*gh.AuthClient, error) {
	if p.authOverride != nil {
		return p.authOverride(conn)
	}
	appID, err := configInt(conn.Config, configAppID)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	installationID, err := configInt(conn.Config, configInstallationID)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	cacheKey := strconv.FormatInt(appID, 10) + ":" + strconv.FormatInt(installationID, 10)

	p.authCacheMu.Lock()
	if cached, ok := p.authCache[cacheKey]; ok {
		p.authCacheMu.Unlock()
		return cached, nil
	}
	p.authCacheMu.Unlock()

	pemBytes, err := p.privateKeyForConnection(conn)
	if err != nil {
		return nil, err
	}
	creds, err := gh.LoadAppCredentials(appID, pemBytes)
	if err != nil {
		return nil, err
	}
	client := gh.NewAuthClient(creds)

	p.authCacheMu.Lock()
	defer p.authCacheMu.Unlock()
	if existing, ok := p.authCache[cacheKey]; ok {
		// Lost the race; use the existing one.
		return existing, nil
	}
	p.authCache[cacheKey] = client
	return client, nil
}

// privateKeyForConnection resolves the PEM-encoded private key from
// the secret store referenced by the connection's credential_refs.
// Tools never see the raw bytes outside this scope.
func (p *Plugin) privateKeyForConnection(conn *domain.Connection) ([]byte, error) {
	if p.secrets == nil {
		return nil, fmt.Errorf("github: secrets store not configured")
	}
	ref, ok := conn.CredentialRefs[credentialPrivateKey]
	if !ok || ref == "" {
		return nil, fmt.Errorf("github: connection %s missing %s in credential_refs", conn.ID, credentialPrivateKey)
	}
	// Resolve handles both real secret:// URIs and the transient
	// pre-migration plaintext shape, matching every other plugin's
	// secret-read path.
	plain, err := secrets.Resolve(p.secrets, ref)
	if err != nil {
		return nil, fmt.Errorf("github: read private key: %w", err)
	}
	return []byte(plain), nil
}

// configInt extracts a JSON-number-flavored config field as int64.
// JSON unmarshaling typically lands integers as float64; we coerce
// gracefully and reject empty / non-numeric values.
func configInt(config map[string]any, key string) (int64, error) {
	raw, ok := config[key]
	if !ok || raw == nil {
		return 0, fmt.Errorf("config key %q missing", key)
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case string:
		if v == "" {
			return 0, fmt.Errorf("config key %q is empty", key)
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("config key %q not a number: %w", key, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("config key %q has unsupported type %T", key, raw)
	}
}
