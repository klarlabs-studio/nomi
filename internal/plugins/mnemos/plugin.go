package mnemos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	mnemosclient "go.klarlabs.de/mnemos/client"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/tools"
)

// Plugin is the Nomi-side Mnemos integration. One Plugin instance
// manages many Connections (one per Mnemos server the user has
// configured) and exposes capability-gated tools + a context source.
type Plugin struct {
	secrets secrets.Store

	mu          sync.RWMutex
	running     bool
	connections map[string]*connection // connection_id -> client wrapper
}

// New constructs the plugin. secretsStore is required for token
// resolution; passing nil causes Configure to refuse credential-bearing
// connections rather than silently dropping the token.
func New(secretsStore secrets.Store) *Plugin {
	return &Plugin{
		secrets:     secretsStore,
		connections: make(map[string]*connection),
	}
}

// connection holds the per-Connection client + the resolved config.
// We hold the client (not just config) so each tool invocation skips
// the build/auth handshake.
type connection struct {
	id                string
	baseURL           string
	visibilityDefault string
	client            *mnemosclient.Client
}

// connectionConfig is the per-Connection JSON shape the runtime feeds
// into Configure. Mirrors PluginManifest.Requires.ConfigSchema +
// Credentials.
type connectionConfig struct {
	ID                string `json:"id"`
	BaseURL           string `json:"base_url"`
	VisibilityDefault string `json:"visibility_default,omitempty"`
	// TokenRef resolves to a bearer token via secrets.Store. Empty for
	// read-only connections.
	TokenRef string `json:"token_ref,omitempty"`
}

// configureInput is the aggregate config the runtime hands the plugin
// on every Configure call. Single object, plural connections.
type configureInput struct {
	Connections []connectionConfig `json:"connections"`
}

// ----- Plugin interface -----

func (p *Plugin) Manifest() plugins.PluginManifest { return buildManifest() }

// Configure reconciles the live connection set with the document the
// runtime hands us. Idempotent: re-calling with the same input is a
// no-op; differences result in adds / removes / replacements.
func (p *Plugin) Configure(ctx context.Context, raw json.RawMessage) error {
	var input configureInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return fmt.Errorf("mnemos plugin: parse config: %w", err)
		}
	}

	desired := make(map[string]*connection, len(input.Connections))
	for _, cc := range input.Connections {
		if cc.ID == "" || cc.BaseURL == "" {
			return fmt.Errorf("mnemos plugin: connection requires id and base_url")
		}
		token := ""
		if cc.TokenRef != "" {
			if p.secrets == nil {
				return fmt.Errorf("mnemos plugin: connection %q wants a token but no secrets store is wired", cc.ID)
			}
			key := cc.TokenRef
			if secrets.IsReference(key) {
				key = key[len(secrets.ReferencePrefix):]
			}
			resolved, err := p.secrets.Get(key)
			if err != nil {
				return fmt.Errorf("mnemos plugin: resolve token for %q: %w", cc.ID, err)
			}
			token = resolved
		}
		visibility := cc.VisibilityDefault
		if visibility == "" {
			visibility = "team"
		}
		opts := []mnemosclient.Option{}
		if token != "" {
			opts = append(opts, mnemosclient.WithToken(token))
		}
		desired[cc.ID] = &connection{
			id:                cc.ID,
			baseURL:           cc.BaseURL,
			visibilityDefault: visibility,
			client:            mnemosclient.New(cc.BaseURL, opts...),
		}
	}

	p.mu.Lock()
	p.connections = desired
	p.mu.Unlock()
	return nil
}

// Start is a no-op for Mnemos. The plugin has no background workers
// (no poll loop, no websocket); tools are stateless per invocation.
// Marking running=true so Status() reports correctly.
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	return nil
}

// Stop is a no-op for the same reason — no resources to release beyond
// the HTTP client connection pools, which Go's net/http manages.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{
		Running: p.running,
		Ready:   p.running && len(p.connections) > 0,
	}
}

// ----- ToolProvider -----

// Tools returns the six tool implementations. Stateless from the
// runtime's perspective; the connection ID arrives as an input
// parameter on each call.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&eventsAppendTool{p: p},
		&claimsAppendTool{p: p},
		&claimsListTool{p: p},
		&relationshipsListTool{p: p},
		&embeddingsAppendTool{p: p},
		&searchTool{p: p},
	}
}

// ----- connection helpers -----

// resolveConnection looks up a connection by ID with the read lock
// held. Returns a not-found error rather than panicking on a missing
// ID — that's the runtime's most common failure mode (user typo in
// tool input).
func (p *Plugin) resolveConnection(connID string) (*connection, error) {
	if connID == "" {
		return nil, errors.New("mnemos: connection_id required")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	c, ok := p.connections[connID]
	if !ok {
		return nil, fmt.Errorf("mnemos: connection %q not configured", connID)
	}
	return c, nil
}
