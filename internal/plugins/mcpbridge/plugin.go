// Package mcpbridge is the generic MCP-server plugin: any stdio or
// HTTP+SSE MCP server becomes a set of Nomi tools. Static dispatch
// (mcp.list_tools / mcp.call) is gated by mcp.tools; each discovered
// upstream tool is gated by its own capability (mcp.<slug>.<tool>)
// so Allow on one tool never covers another.
//
// Scout (com.nomi.scout) remains the opinionated browser-shaped
// client with a fixed six-tool surface. This plugin is the
// config-driven follow-up: point it at filesystem, github, postgres,
// or any other MCP server and the planner sees the discovered tools.
package mcpbridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	mcpclient "go.klarlabs.de/mcp/client"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.mcp"

// Capability is the permission string for the static dispatch tools
// (mcp.list_tools / mcp.call). Discovered upstream tools use a
// per-tool capability equal to their registry name
// (mcp.<connection-slug>.<tool>) so Allow on one tool never silently
// covers another — Goose/Cline parity on fine-grained MCP consent.
const Capability = "mcp.tools"

const maxDiscoveredPerConnection = 32

// mcpSession is the subset of *mcpclient.Client the plugin needs.
// Tests inject a stub so we don't spawn a real subprocess.
type mcpSession interface {
	ListTools(ctx context.Context) ([]mcpclient.ToolInfo, error)
	CallTool(ctx context.Context, name string, arguments any) (*mcpclient.ToolResult, error)
	Close() error
}

// Plugin implements plugins.Plugin + ToolProvider + ConnectionHealthReporter.
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	secrets     secrets.Store
	toolsReg    *tools.Registry

	sessionFactory func(ctx context.Context, conn *domain.Connection) (mcpSession, error)

	mu              sync.RWMutex
	running         bool
	sessions        map[string]mcpSession
	health          map[string]*plugins.ConnectionHealth
	discovered      map[string][]mcpclient.ToolInfo // connection_id -> upstream tools
	registeredNames map[string][]string             // connection_id -> names in toolsReg
}

// NewPlugin wires the generic MCP plugin. toolsReg may be nil (tests);
// when set, discovered upstream tools are hot-registered so the planner
// sees them by name (mcp.<slug>.<tool>) in addition to mcp.call.
func NewPlugin(
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	secretStore secrets.Store,
	toolsReg *tools.Registry,
) *Plugin {
	return &Plugin{
		connections:     conns,
		bindings:        binds,
		secrets:         secretStore,
		toolsReg:        toolsReg,
		sessions:        map[string]mcpSession{},
		health:          map[string]*plugins.ConnectionHealth{},
		discovered:      map[string][]mcpclient.ToolInfo{},
		registeredNames: map[string][]string{},
	}
}

// SetSessionFactory is a test seam. Production uses the default which
// builds a real MCP client from the connection config.
func (p *Plugin) SetSessionFactory(fn func(ctx context.Context, conn *domain.Connection) (mcpSession, error)) {
	p.sessionFactory = fn
}

// Manifest declares the static dispatch surface. Discovered tools are
// not listed here — they are registered into tools.Registry at Start
// (and on connection restart) so the plugin can stay connection-multi
// without a combinatorial manifest.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "MCP Server",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Connect any MCP server (stdio or HTTP+SSE). Discovered tools are gated per-tool (mcp.<connection>.<tool>) and surface in plan review. Use this to match OpenClaw/Goose/Cline plugin breadth without writing a Nomi plugin.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			Capability,
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{Name: "mcp.list_tools", Capability: Capability, Description: "List tools exposed by a connected MCP server.", RequiresConnection: true},
				{Name: "mcp.call", Capability: Capability, Description: "Call a named tool on a connected MCP server.", RequiresConnection: true},
			},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{
					Kind:        "mcp_bearer_token",
					Key:         "token",
					Label:       "Bearer token (HTTP transport only)",
					Required:    false,
					Description: "Optional. Set when the MCP server requires Authorization: Bearer.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				"transport": {
					Type: "enum", Label: "Transport",
					Default:     "stdio",
					Description: "How Nomi reaches the MCP server.",
					Options: []plugins.ConfigOption{
						{Value: "stdio", Label: "stdio (spawn local binary)"},
						{Value: "http", Label: "HTTP + SSE (remote server)"},
					},
				},
				"command": {
					Type: "string", Label: "Command (stdio)",
					Description: "Path to the MCP server binary. Used only when transport=stdio.",
				},
				"args": {
					Type: "string", Label: "Args (stdio, comma- or space-separated)",
					Description: "CLI args passed to the binary, e.g. `run --dir /path`.",
				},
				"endpoint": {
					Type: "string", Label: "Endpoint (http)",
					Description: "Base URL of the MCP server. Used only when transport=http.",
				},
			},
		},
	}
}

func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

func (p *Plugin) ConnectionHealth(connectionID string) (plugins.ConnectionHealth, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.health[connectionID]
	if !ok || h == nil {
		return plugins.ConnectionHealth{}, false
	}
	out := *h
	if names := p.registeredNames[connectionID]; len(names) > 0 {
		out.DiscoveredTools = append([]string(nil), names...)
	}
	return out, true
}

// Start marks the plugin running and discovers tools on every enabled
// connection. Discovery failures are recorded as connection health
// errors and do not fail Start — a missing binary must not block boot.
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()

	if p.connections == nil {
		return nil
	}
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		return nil
	}
	for _, conn := range conns {
		if conn == nil || !conn.Enabled {
			continue
		}
		if err := p.discover(ctx, conn); err != nil {
			p.recordError(conn.ID, err.Error())
			slog.Warn("mcp: discovery failed", "connection_id", conn.ID, "error", err)
		}
	}
	return nil
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range p.sessions {
		if s != nil {
			_ = s.Close()
		}
		delete(p.sessions, id)
	}
	if p.toolsReg != nil {
		for _, names := range p.registeredNames {
			for _, n := range names {
				p.toolsReg.Unregister(n)
			}
		}
	}
	p.registeredNames = map[string][]string{}
	p.discovered = map[string][]mcpclient.ToolInfo{}
	p.running = false
	return nil
}

func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&dispatchTool{plugin: p, name: "mcp.list_tools", kind: dispatchList},
		&dispatchTool{plugin: p, name: "mcp.call", kind: dispatchCall},
	}
}

var _ plugins.Plugin = (*Plugin)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ plugins.ConnectionHealthReporter = (*Plugin)(nil)
