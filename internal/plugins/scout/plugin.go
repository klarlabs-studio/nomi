// Package scout integrates the Scout browser-automation MCP server as
// a first-party Nomi plugin. Spawns `scout` over stdio (or talks to a
// running scout HTTP+SSE server), discovers its tool surface via the
// MCP handshake, and exposes the most commonly-used six tools through
// Nomi's tools.Registry. Each exposed tool routes through the same
// capability engine + plan-review gate that protects the runtime's
// built-in tools.
//
// The plugin is intentionally Scout-specific for v1 even though
// `github.com/felixgeelhaar/mcp-go/client` is a generic client. The
// generic "any-MCP-server" plugin variant is a follow-up — see the
// roady backlog item.
package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mcpclient "github.com/felixgeelhaar/mcp-go/client"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/plugins"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.scout"

// Plugin implements plugins.Plugin + plugins.ToolProvider + Connection-
// HealthReporter. Each Connection maps to one Scout process / endpoint;
// every tool call resolves the connection at invocation time so a
// dropped subprocess can be reaped and reconnected on the next call.
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	secrets     secrets.Store

	mu      sync.RWMutex
	running bool
	clients map[string]*mcpclient.Client    // connection_id -> client
	health  map[string]*plugins.ConnectionHealth
}

// NewPlugin wires the Scout plugin.
func NewPlugin(
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		connections: conns,
		bindings:    binds,
		secrets:     secretStore,
		clients:     map[string]*mcpclient.Client{},
		health:      map[string]*plugins.ConnectionHealth{},
	}
}

// Manifest declares the Scout plugin's contract. The tool set is
// fixed — these are the six Scout primitives we route through the
// capability engine. Adding more is one struct entry + one
// tools.Tool dispatch in tools.go.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Scout (browser)",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Browser automation via the Scout MCP server. Navigate, observe, click, type, screenshot, extract — each call gated by Nomi's capability engine.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"scout.browse",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{Name: "scout.navigate", Capability: "scout.browse", Description: "Navigate the browser to a URL.", RequiresConnection: true},
				{Name: "scout.observe", Capability: "scout.browse", Description: "Return an annotated screenshot listing every interactive element on the current page.", RequiresConnection: true},
				{Name: "scout.click", Capability: "scout.browse", Description: "Click an element by CSS selector.", RequiresConnection: true},
				{Name: "scout.type", Capability: "scout.browse", Description: "Type text into an input by CSS selector.", RequiresConnection: true},
				{Name: "scout.screenshot", Capability: "scout.browse", Description: "Capture a screenshot of the current page.", RequiresConnection: true},
				{Name: "scout.extract", Capability: "scout.browse", Description: "Extract the text content matching a CSS selector.", RequiresConnection: true},
			},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{
					Kind:        "scout_bearer_token",
					Key:         "token",
					Label:       "Scout bearer token (HTTP transport only)",
					Required:    false,
					Description: "Optional. Set only when the Scout server runs over HTTP+SSE and requires auth.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				"transport": {
					Type: "enum", Label: "Transport",
					Default: "stdio",
					Description: "How Nomi reaches the Scout MCP server.",
					Options: []plugins.ConfigOption{
						{Value: "stdio", Label: "stdio (spawn local binary)"},
						{Value: "http", Label: "HTTP + SSE (remote server)"},
					},
				},
				"command": {
					Type: "string", Label: "Command (stdio)",
					Default:     "scout",
					Description: "Path to the scout binary (or `scout-mcp`). Used only when transport=stdio.",
				},
				"args": {
					Type: "string", Label: "Args (stdio, comma-separated)",
					Default:     "mcp",
					Description: "CLI args passed to the scout binary. Defaults to the `mcp` subcommand.",
				},
				"endpoint": {
					Type: "string", Label: "Endpoint (http)",
					Description: "Base URL of the Scout MCP server (HTTP+SSE). Used only when transport=http.",
				},
			},
			NetworkAllowlist: nil, // Endpoint is per-connection; scout itself drives whatever URL the assistant navigates to.
		},
	}
}

// Configure persists the connection list; lazy client construction
// happens on first tool call so a misconfigured connection doesn't
// crash Start.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Status returns plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// ConnectionHealth implements plugins.ConnectionHealthReporter.
func (p *Plugin) ConnectionHealth(connectionID string) (plugins.ConnectionHealth, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.health[connectionID]
	if !ok || h == nil {
		return plugins.ConnectionHealth{}, false
	}
	return *h, true
}

// Start marks the plugin running. No eager connect: clients are
// resolved lazily on first tool call so a Scout binary that isn't
// installed doesn't block daemon boot.
func (p *Plugin) Start(_ context.Context) error {
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()
	return nil
}

// Stop closes every cached MCP client.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, c := range p.clients {
		if c != nil {
			_ = c.Close()
		}
		delete(p.clients, id)
	}
	p.running = false
	return nil
}

// resolveClient fetches (or builds) an initialised MCP client for the
// named connection. Reused across tool calls so the stdio subprocess
// (or HTTP session) doesn't restart per invocation.
func (p *Plugin) resolveClient(ctx context.Context, connectionID string) (*mcpclient.Client, error) {
	p.mu.RLock()
	c := p.clients[connectionID]
	p.mu.RUnlock()
	if c != nil {
		return c, nil
	}

	conn, err := p.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("scout: connection %s: %w", connectionID, err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("scout: connection %s is disabled", connectionID)
	}

	transport, err := p.buildTransport(conn)
	if err != nil {
		p.recordError(connectionID, err.Error())
		return nil, err
	}

	client := mcpclient.New(transport,
		mcpclient.WithTimeout(30*time.Second),
		mcpclient.WithClientInfo("nomi-scout-plugin", "0.1.0"),
	)
	if _, err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		p.recordError(connectionID, err.Error())
		return nil, fmt.Errorf("scout: initialize: %w", err)
	}

	p.mu.Lock()
	p.clients[connectionID] = client
	p.health[connectionID] = &plugins.ConnectionHealth{Running: true, LastEventAt: time.Now().UTC()}
	p.mu.Unlock()
	return client, nil
}

func (p *Plugin) buildTransport(conn *domain.Connection) (mcpclient.Transport, error) {
	transport, _ := conn.Config["transport"].(string)
	if transport == "" {
		transport = "stdio"
	}
	switch transport {
	case "stdio":
		cmd, _ := conn.Config["command"].(string)
		if cmd == "" {
			cmd = "scout"
		}
		argsCSV, _ := conn.Config["args"].(string)
		args := splitArgs(argsCSV)
		return mcpclient.NewStdioTransport(cmd, args...)
	case "http":
		endpoint, _ := conn.Config["endpoint"].(string)
		if endpoint == "" {
			return nil, fmt.Errorf("scout: http transport requires endpoint config")
		}
		opts := []mcpclient.HTTPTransportOption{}
		if ref, ok := conn.CredentialRefs["token"]; ok && ref != "" {
			tok, err := secrets.Resolve(p.secrets, ref)
			if err != nil {
				return nil, fmt.Errorf("scout: resolve token: %w", err)
			}
			if tok != "" {
				opts = append(opts, mcpclient.WithBearerToken(tok))
			}
		}
		return mcpclient.NewHTTPTransport(endpoint, opts...)
	default:
		return nil, fmt.Errorf("scout: unknown transport %q", transport)
	}
}

func (p *Plugin) recordError(connectionID, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.health[connectionID]
	if !ok {
		h = &plugins.ConnectionHealth{}
		p.health[connectionID] = h
	}
	h.LastError = msg
	h.ErrorCount++
	slog.Error("scout: connection error", "connection_id", connectionID, "error", msg)
}

func (p *Plugin) recordActivity(connectionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.health[connectionID]
	if !ok {
		h = &plugins.ConnectionHealth{Running: true}
		p.health[connectionID] = h
	}
	h.LastEventAt = time.Now().UTC()
	h.LastError = ""
	h.ErrorCount = 0
}

// splitArgs is a comma-then-trim split. Quoted strings with embedded
// commas aren't supported; the args field is for simple invocations
// like "mcp" or "mcp --port 7777".
func splitArgs(csv string) []string {
	if csv == "" {
		return nil
	}
	out := []string{}
	cur := []rune{}
	flush := func() {
		s := string(cur)
		cur = cur[:0]
		s = trimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	for _, r := range csv {
		switch r {
		case ',', ' ':
			flush()
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// Tools implements plugins.ToolProvider — returns the six Scout
// primitives. Each is a thin wrapper that resolves the connection on
// call and routes through the cached MCP client.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&scoutTool{plugin: p, name: "scout.navigate", upstream: "navigate"},
		&scoutTool{plugin: p, name: "scout.observe", upstream: "annotated_screenshot"},
		&scoutTool{plugin: p, name: "scout.click", upstream: "click"},
		&scoutTool{plugin: p, name: "scout.type", upstream: "type"},
		&scoutTool{plugin: p, name: "scout.screenshot", upstream: "screenshot"},
		&scoutTool{plugin: p, name: "scout.extract", upstream: "extract"},
	}
}

// Compile-time interface guards.
var _ plugins.Plugin = (*Plugin)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ plugins.ConnectionHealthReporter = (*Plugin)(nil)
