package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
)

// browserTool is the per-Tool dispatcher. Every tool in the plugin
// is one of these — they differ only in (Nomi tool name, capability,
// Scout MCP tool name, arg-shape mapping).
type browserTool struct {
	plugin  *Plugin
	name    string
	cap     string
	mcpName string
	argMap  func(map[string]interface{}) map[string]interface{}
}

func (t *browserTool) Name() string       { return t.name }
func (t *browserTool) Capability() string { return t.cap }

func (t *browserTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("%s: connection_id is required", t.name)
	}
	if assistantID, _ := input["__assistant_id"].(string); assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("%s: binding check failed: %w", t.name, err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}
	conn, err := t.plugin.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s: connection %s is disabled", t.name, connectionID)
	}

	// browser.navigate is the only tool that takes a URL as input —
	// gate it against the connection's allowed_hosts list. Other
	// tools (click/type/extract/etc.) operate on whatever page is
	// already loaded; once you got there via a gated navigate, the
	// page is implicitly trusted. This matches how users reason
	// about browser security: "I told it to go to example.com;
	// clicking buttons there is normal."
	if t.name == "browser.navigate" {
		urlStr, _ := input["url"].(string)
		if err := gateNavigate(conn, urlStr); err != nil {
			return nil, fmt.Errorf("%s: %w", t.name, err)
		}
	}

	client, err := t.plugin.clientFor(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}

	args := t.argMap(input)
	rawResult, err := client.CallTool(ctx, t.mcpName, args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	return decodeMCPResult(rawResult), nil
}

// decodeMCPResult unwraps the standard MCP `{content: [{type:
// "text", text: "..."}]}` envelope into something a Nomi assistant
// can consume. Scout returns either a JSON-stringified payload or
// plain text; we try JSON first and fall back to a `{text: ...}`
// wrapper so the tool result is always an object (the planner
// expects map[string]interface{}, never raw scalars).
func decodeMCPResult(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{}
	}
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		// Not the standard MCP shape — surface the raw JSON under
		// `result` so callers can still see what came back.
		return map[string]interface{}{"result": json.RawMessage(raw)}
	}
	if len(envelope.Content) == 0 {
		return map[string]interface{}{"is_error": envelope.IsError}
	}
	// Concatenate text parts; Scout typically returns one part per call.
	combined := ""
	for _, c := range envelope.Content {
		combined += c.Text
	}
	// Try to parse the text as JSON — most Scout tools return
	// JSON-stringified payloads (the elements list from observe,
	// the tabular data from extract_table, etc.). Fall back to a
	// plain-text wrapper.
	var asJSON interface{}
	if err := json.Unmarshal([]byte(combined), &asJSON); err == nil {
		if asMap, ok := asJSON.(map[string]interface{}); ok {
			if envelope.IsError {
				asMap["is_error"] = true
			}
			return asMap
		}
		return map[string]interface{}{"result": asJSON, "is_error": envelope.IsError}
	}
	return map[string]interface{}{"text": combined, "is_error": envelope.IsError}
}
