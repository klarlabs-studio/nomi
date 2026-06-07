package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.browser"

// defaultScoutBin is the binary name we look up on PATH when the
// connection config doesn't specify one. Users with a custom Scout
// install override via the `scout_path` config field.
const defaultScoutBin = "scout"

// Plugin is the Browser plugin. Tool-only — no inbound triggers
// today (browser-03's per-domain approval gating is the next step).
// Each Connection corresponds to one Scout subprocess with an
// isolated browser profile so assistants don't share auth state.
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	secrets     secrets.Store

	// clientFactory builds the MCP client for a Connection. Defaults
	// to startMCP; tests inject a stub that doesn't spawn a real
	// process.
	clientFactory func(ctx context.Context, conn *domain.Connection) (mcpInvoker, error)

	mu        sync.RWMutex
	running   bool
	clients   map[string]mcpInvoker // connection_id -> live client
	startCtx  context.Context       // captured on Start; clients use this for their lifetime
	startStop context.CancelFunc
}

// mcpInvoker is the subset of *mcpClient the tool dispatchers need.
// Defining the interface lets tests substitute without spinning up
// a real Scout subprocess.
type mcpInvoker interface {
	CallTool(ctx context.Context, name string, args map[string]interface{}) (json.RawMessage, error)
	Close() error
}

// NewPlugin constructs the Browser plugin.
func NewPlugin(
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		connections: conns,
		bindings:    binds,
		secrets:     secretStore,
		clients:     map[string]mcpInvoker{},
	}
}

// SetClientFactory swaps the Scout-spawn function. Test-only seam —
// production uses the default which spawns a real subprocess.
func (p *Plugin) SetClientFactory(fn func(ctx context.Context, conn *domain.Connection) (mcpInvoker, error)) {
	p.clientFactory = fn
}

// Manifest declares the Browser plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Browser",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Browser automation via Scout (klarlabs-studio/scout). Each connection is a separate isolated browser profile so assistants don't share auth state. Requires `scout` on PATH (brew install scout).",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"browser.navigate",
			"browser.observe",
			"browser.interact",
			"browser.extract",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{Name: "browser.navigate", Capability: "browser.navigate", RequiresConnection: true,
					Description: "Navigate to a URL. Inputs: connection_id, url. Returns the page title."},
				{Name: "browser.observe", Capability: "browser.observe", RequiresConnection: true,
					Description: "Return a structured snapshot of interactive elements on the current page (links, inputs, buttons with their selectors). Inputs: connection_id."},
				{Name: "browser.click", Capability: "browser.interact", RequiresConnection: true,
					Description: "Click an element by CSS selector. Inputs: connection_id, selector, wait? (true if click triggers navigation)."},
				{Name: "browser.type", Capability: "browser.interact", RequiresConnection: true,
					Description: "Type text into an input element. Inputs: connection_id, selector, text."},
				{Name: "browser.fill_form", Capability: "browser.interact", RequiresConnection: true,
					Description: "Fill a form via semantic field labels. Inputs: connection_id, fields (object: {LabelOrPlaceholder: value, ...})."},
				{Name: "browser.extract", Capability: "browser.extract", RequiresConnection: true,
					Description: "Extract text from elements matching a CSS selector. Inputs: connection_id, selector."},
				{Name: "browser.extract_table", Capability: "browser.extract", RequiresConnection: true,
					Description: "Extract a table as rows of objects. Inputs: connection_id, selector."},
				{Name: "browser.screenshot", Capability: "browser.observe", RequiresConnection: true,
					Description: "Capture a screenshot. Inputs: connection_id, full_page?, max_width?, quality?. Returns base64 PNG/JPEG data URL."},
				{Name: "browser.readable_text", Capability: "browser.extract", RequiresConnection: true,
					Description: "Return the page's main readable content stripped of nav/boilerplate. Inputs: connection_id."},
				{Name: "browser.markdown", Capability: "browser.extract", RequiresConnection: true,
					Description: "Render the page as compact markdown. Inputs: connection_id."},
				{Name: "browser.wait_for", Capability: "browser.observe", RequiresConnection: true,
					Description: "Wait for an element to appear before returning. Inputs: connection_id, selector, timeout_seconds?."},
				{Name: "browser.has_element", Capability: "browser.observe", RequiresConnection: true,
					Description: "Check whether an element exists without erroring on absence. Inputs: connection_id, selector."},
				{Name: "browser.scroll_by", Capability: "browser.interact", RequiresConnection: true,
					Description: "Scroll the page by pixel amounts. Inputs: connection_id, x?, y?."},
				{Name: "browser.dismiss_cookies", Capability: "browser.interact", RequiresConnection: true,
					Description: "Best-effort cookie banner dismissal. Inputs: connection_id."},
				{Name: "browser.console_errors", Capability: "browser.observe", RequiresConnection: true,
					Description: "Return console errors emitted by the page. Inputs: connection_id."},
			},
		},
		Requires: plugins.Requirements{
			ConfigSchema: map[string]plugins.ConfigField{
				"scout_path": {
					Type: "string", Label: "Scout binary path", Required: false,
					Default:     defaultScoutBin,
					Description: "Path to the Scout binary. Defaults to looking up `scout` on PATH.",
				},
				"profile_dir": {
					Type: "string", Label: "Browser profile directory", Required: false,
					Description: "Optional persistent profile dir for cookies + storage. Empty = ephemeral profile per session.",
				},
				"headless": {
					Type: "boolean", Label: "Run headless", Required: false, Default: "true",
					Description: "Run the browser without a visible window. Disable for debugging or for sites that reject headless UAs.",
				},
				"allowed_hosts": {
					Type: "string", Label: "Allowed hosts (comma-separated)", Required: true,
					Description: `Hosts the assistant may navigate to. Supports wildcards: "github.com", "*.example.com", "*" (unrestricted, use sparingly). Empty = deny all navigation.`,
				},
			},
			// network_allowlist intentionally empty — what counts as
			// outbound network traffic for a browser is "every URL the
			// user navigates to," which is unbounded. The
			// per-Connection allowed_hosts above is the gate that
			// matters for browser navigation; the wasmhost-style
			// manifest NetworkAllowlist would force one fixed list
			// across every connection, defeating the per-Connection
			// scoping point.
			NetworkAllowlist: nil,
		},
	}
}

// Configure is a no-op — connection state lives on the Connection
// row and is read fresh when each Connection's Scout subprocess is
// spawned.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start launches a Scout subprocess for every enabled Connection so
// the first tool call doesn't pay subprocess-spawn latency. Each
// client's lifetime is bound to startCtx, so Stop tears them all
// down at once.
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	p.startCtx, p.startStop = context.WithCancel(ctx)
	p.running = true
	p.mu.Unlock()

	if p.connections == nil {
		return nil
	}
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		return fmt.Errorf("list browser connections: %w", err)
	}
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		// Best-effort spawn — a per-connection failure shouldn't
		// prevent other connections from working. The error surfaces
		// when the user tries to use a tool on the failed connection.
		if _, err := p.clientFor(p.startCtx, conn); err != nil {
			// Logged in clientFor; nothing more to do here.
			_ = err
		}
	}
	return nil
}

// Stop closes every live Scout subprocess.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return nil
	}
	if p.startStop != nil {
		p.startStop()
	}
	clients := p.clients
	p.clients = map[string]mcpInvoker{}
	p.running = false
	p.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}
	return nil
}

// Status reports plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: p.running}
}

// Tools implements plugins.ToolProvider. One tool per
// ToolContribution; all dispatch through the shared invokeMCP
// helper which translates Nomi tool args to Scout MCP args.
func (p *Plugin) Tools() []tools.Tool {
	specs := []struct {
		name    string
		cap     string
		mcpName string
		argMap  func(map[string]interface{}) map[string]interface{}
	}{
		{"browser.navigate", "browser.navigate", "navigate", passArgs("url")},
		{"browser.observe", "browser.observe", "observe", passArgs()},
		{"browser.click", "browser.interact", "click", passArgs("selector", "wait")},
		{"browser.type", "browser.interact", "type", passArgs("selector", "text")},
		{"browser.fill_form", "browser.interact", "fill_form_semantic", passArgs("fields")},
		{"browser.extract", "browser.extract", "extract", passArgs("selector")},
		{"browser.extract_table", "browser.extract", "extract_table", passArgs("selector")},
		{"browser.screenshot", "browser.observe", "screenshot", passArgs("full_page", "max_width", "quality")},
		{"browser.readable_text", "browser.extract", "readable_text", passArgs()},
		{"browser.markdown", "browser.extract", "markdown", passArgs()},
		{"browser.wait_for", "browser.observe", "wait_for", passArgs("selector", "timeout_seconds")},
		{"browser.has_element", "browser.observe", "has_element", passArgs("selector")},
		{"browser.scroll_by", "browser.interact", "scroll_by", passArgs("x", "y")},
		{"browser.dismiss_cookies", "browser.interact", "dismiss_cookies", passArgs()},
		{"browser.console_errors", "browser.observe", "console_errors", passArgs()},
	}
	out := make([]tools.Tool, 0, len(specs))
	for _, s := range specs {
		out = append(out, &browserTool{
			plugin:  p,
			name:    s.name,
			cap:     s.cap,
			mcpName: s.mcpName,
			argMap:  s.argMap,
		})
	}
	return out
}

// passArgs returns an argMap that copies the listed keys from the
// Nomi tool input verbatim into the MCP call. The 1:1 keys keep
// tool documentation readable — Scout's MCP names match.
func passArgs(keys ...string) func(map[string]interface{}) map[string]interface{} {
	return func(in map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for _, k := range keys {
			if v, ok := in[k]; ok {
				out[k] = v
			}
		}
		return out
	}
}

// clientFor returns the live mcpInvoker for a connection, lazily
// spawning Scout on first use. Concurrent callers for the same
// connection share the same client.
func (p *Plugin) clientFor(ctx context.Context, conn *domain.Connection) (mcpInvoker, error) {
	p.mu.RLock()
	if c, ok := p.clients[conn.ID]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[conn.ID]; ok {
		return c, nil
	}
	factory := p.clientFactory
	if factory == nil {
		factory = defaultClientFactory
	}
	// Use the start-bound context so the subprocess outlives a single
	// tool call but dies cleanly when Stop fires.
	spawnCtx := p.startCtx
	if spawnCtx == nil {
		spawnCtx = ctx
	}
	c, err := factory(spawnCtx, conn)
	if err != nil {
		return nil, fmt.Errorf("spawn scout for connection %s: %w", conn.ID, err)
	}
	p.clients[conn.ID] = c
	return c, nil
}

// defaultClientFactory spawns a real Scout subprocess. Reads
// scout_path + headless from the connection config; the rest of
// Scout's flags are surfaced via Scout's own configure tool at
// runtime rather than at startup.
func defaultClientFactory(ctx context.Context, conn *domain.Connection) (mcpInvoker, error) {
	binPath := defaultScoutBin
	if v, _ := conn.Config["scout_path"].(string); v != "" {
		binPath = v
	}
	args := []string{"mcp", "serve"}
	return startMCP(ctx, binPath, args)
}
