package mcpbridge

import (
	"context"
	"fmt"
	"strings"

	mcpclient "go.klarlabs.de/mcp/client"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

type dispatchKind int

const (
	dispatchList dispatchKind = iota
	dispatchCall
)

type dispatchTool struct {
	plugin *Plugin
	name   string
	kind   dispatchKind
}

func (t *dispatchTool) Name() string        { return t.name }
func (t *dispatchTool) Capability() string  { return Capability }
func (t *dispatchTool) Description() string { return toolDescription(t.name) }

func toolDescription(name string) string {
	switch name {
	case "mcp.list_tools":
		return "List tools exposed by a connected MCP server."
	case "mcp.call":
		return "Call a named tool on a connected MCP server. Requires user approval."
	default:
		return ""
	}
}

func (t *dispatchTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	conn, err := t.plugin.resolveConnection(input, t.name)
	if err != nil {
		return nil, err
	}
	session, err := t.plugin.resolveSession(ctx, conn)
	if err != nil {
		return nil, err
	}

	switch t.kind {
	case dispatchList:
		infos, err := session.ListTools(ctx)
		if err != nil {
			t.plugin.recordError(conn.ID, err.Error())
			return nil, fmt.Errorf("mcp.list_tools: %w", err)
		}
		t.plugin.recordActivity(conn.ID)
		out := make([]map[string]interface{}, 0, len(infos))
		for _, info := range infos {
			entry := map[string]interface{}{
				"name":        info.Name,
				"description": info.Description,
			}
			if info.InputSchema != nil {
				entry["input_schema"] = info.InputSchema
			}
			out = append(out, entry)
		}
		return map[string]interface{}{"tools": out, "connection_id": conn.ID}, nil
	case dispatchCall:
		toolName, _ := input["tool"].(string)
		if toolName == "" {
			return nil, fmt.Errorf("mcp.call: tool is required")
		}
		args := map[string]interface{}{}
		if raw, ok := input["arguments"].(map[string]interface{}); ok && raw != nil {
			args = raw
		}
		return t.plugin.callUpstream(ctx, session, conn.ID, toolName, args)
	default:
		return nil, fmt.Errorf("mcp: unknown dispatch")
	}
}

// discoveredTool is a hot-registered wrapper around one upstream MCP tool.
type discoveredTool struct {
	plugin      *Plugin
	connID      string
	nomiName    string
	upstream    string
	description string
	schema      any
}

func (t *discoveredTool) Name() string { return t.nomiName }

// Capability is the registry name (mcp.<slug>.<tool>). Remembered
// approvals and policy rules therefore apply per discovered tool, not
// to the whole MCP connection.
func (t *discoveredTool) Capability() string  { return t.nomiName }
func (t *discoveredTool) Description() string { return t.description }
func (t *discoveredTool) InputSchema() any    { return t.schema }

func (t *discoveredTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	if input == nil {
		input = map[string]interface{}{}
	}
	if _, ok := input["connection_id"].(string); !ok || input["connection_id"] == "" {
		input["connection_id"] = t.connID
	}
	conn, err := t.plugin.resolveConnection(input, t.nomiName)
	if err != nil {
		return nil, err
	}
	session, err := t.plugin.resolveSession(ctx, conn)
	if err != nil {
		return nil, err
	}
	args := map[string]interface{}{}
	for k, v := range input {
		if k == "connection_id" || strings.HasPrefix(k, "__") || k == "command" {
			continue
		}
		args[k] = v
	}
	return t.plugin.callUpstream(ctx, session, conn.ID, t.upstream, args)
}

func (p *Plugin) callUpstream(ctx context.Context, session mcpSession, connectionID, toolName string, args map[string]interface{}) (map[string]interface{}, error) {
	result, err := session.CallTool(ctx, toolName, args)
	if err != nil {
		p.recordError(connectionID, err.Error())
		return nil, fmt.Errorf("mcp.call %s: %w", toolName, err)
	}
	p.recordActivity(connectionID)
	return flattenToolResult(result), nil
}

func flattenToolResult(result *mcpclient.ToolResult) map[string]interface{} {
	out := map[string]interface{}{"is_error": false}
	if result == nil {
		return out
	}
	out["is_error"] = result.IsError
	textParts := []string{}
	for _, c := range result.Content {
		if c.Type == "text" && c.Text != "" {
			textParts = append(textParts, c.Text)
		}
		if c.Type == "image" && c.Data != "" {
			out["image_data"] = c.Data
		}
	}
	if len(textParts) > 0 {
		out["text"] = strings.Join(textParts, "\n")
	}
	out["content"] = result.Content
	if len(result.StructuredContent) > 0 {
		out["structured"] = jsonRawAsAny(result.StructuredContent)
	}
	return out
}

func jsonRawAsAny(raw []byte) interface{} {
	var v interface{}
	if err := jsonUnmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func (p *Plugin) resolveConnection(input map[string]interface{}, toolName string) (*domain.Connection, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		id, err := p.autoConnectionID(input)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", toolName, err)
		}
		connectionID = id
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
		return nil, fmt.Errorf("%s: no connection repository", toolName)
	}
	conn, err := p.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", toolName, err)
	}
	if conn.PluginID != PluginID {
		return nil, fmt.Errorf("%s: connection %s is not an MCP server", toolName, connectionID)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s: connection %s is disabled", toolName, connectionID)
	}
	return conn, nil
}

func (p *Plugin) autoConnectionID(input map[string]interface{}) (string, error) {
	assistantID, _ := input["__assistant_id"].(string)
	if assistantID == "" || p.bindings == nil || p.connections == nil {
		return "", fmt.Errorf("connection_id is required")
	}
	bindings, err := p.bindings.ListByAssistant(assistantID)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, b := range bindings {
		if b == nil || !b.Enabled || b.Role != domain.BindingRoleTool {
			continue
		}
		conn, err := p.connections.GetByID(b.ConnectionID)
		if err != nil || conn.PluginID != PluginID || !conn.Enabled {
			continue
		}
		ids = append(ids, conn.ID)
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("no MCP connection bound to this assistant")
	default:
		return "", fmt.Errorf("multiple MCP connections bound; pass connection_id")
	}
}
