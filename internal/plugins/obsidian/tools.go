package obsidian

import (
	"context"
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/tools"
)

// toolDef bundles the manifest-facing metadata and the runtime closure
// for a single Obsidian tool. Mirrors the github plugin's approach so
// the manifest contribution and the tools.Tool implementation render
// from one source of truth.
type toolDef struct {
	name       string
	capability string
	desc       string
	run        func(ctx context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error)
}

func (t toolDef) asContribution() plugins.ToolContribution {
	return plugins.ToolContribution{
		Name:               t.name,
		Capability:         t.capability,
		RequiresConnection: true,
		Description:        t.desc,
	}
}

type pluginTool struct {
	plugin *Plugin
	def    toolDef
}

func (p *pluginTool) Name() string       { return p.def.name }
func (p *pluginTool) Capability() string { return p.def.capability }
func (p *pluginTool) Execute(ctx context.Context, input map[string]any) (map[string]any, error) {
	conn, err := p.plugin.resolveConnection(input, p.def.name)
	if err != nil {
		return nil, err
	}
	return p.def.run(ctx, conn, input)
}

// Tools implements plugins.ToolProvider. Aggregates every tool family
// the plugin supports.
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
	all = append(all, p.noteTools()...)
	all = append(all, p.searchTools()...)
	all = append(all, p.linkTools()...)
	return all
}

func (p *Plugin) toolContributions() []plugins.ToolContribution {
	defs := p.allToolDefs()
	out := make([]plugins.ToolContribution, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.asContribution())
	}
	return out
}

// resolveConnection performs the connection_id presence + binding +
// enabled checks shared by every Obsidian tool. Returns the live
// Connection on success. Mirrors the github plugin's implementation
// so the security posture is uniform across plugins.
func (p *Plugin) resolveConnection(input map[string]any, toolName string) (*domain.Connection, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("%s: connection_id is required", toolName)
	}
	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && p.bindings != nil {
		ok, err := p.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("%s: binding check failed: %w", toolName, err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}
	if p.connections == nil {
		return nil, fmt.Errorf("%s: connection repository not configured", toolName)
	}
	conn, err := p.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", toolName, err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s: connection %s is disabled", toolName, connectionID)
	}
	return conn, nil
}

// stringInput returns the trimmed string at key or "" when missing.
func stringInput(input map[string]any, key string) string {
	s, _ := input[key].(string)
	return s
}

// stringSliceInput accepts either []string, []any, or comma-separated
// string at key. Empty slice on absence.
func stringSliceInput(input map[string]any, key string) []string {
	switch v := input[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		out := []string{}
		for _, p := range strings.Split(v, ",") {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

func intInput(input map[string]any, key string, def int) int {
	switch v := input[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

func boolInput(input map[string]any, key string, def bool) bool {
	if v, ok := input[key].(bool); ok {
		return v
	}
	return def
}
