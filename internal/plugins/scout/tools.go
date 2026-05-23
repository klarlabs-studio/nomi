package scout

import (
	"context"
	"fmt"
	"strings"
)

// scoutTool bridges a Nomi tools.Tool invocation to an MCP CallTool.
// `name` is the Nomi-side tool name (e.g. "scout.navigate"); `upstream`
// is the Scout MCP server's tool name (e.g. "navigate" /
// "annotated_screenshot"). Input is passed through unchanged after
// stripping Nomi-reserved keys.
type scoutTool struct {
	plugin   *Plugin
	name     string
	upstream string
}

func (t *scoutTool) Name() string       { return t.name }
func (t *scoutTool) Capability() string { return "scout.browse" }

func (t *scoutTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("%s: connection_id is required", t.name)
	}

	client, err := t.plugin.resolveClient(ctx, connectionID)
	if err != nil {
		return nil, err
	}

	// Strip Nomi-reserved input keys before forwarding to Scout. The
	// runtime injects __* prefixed escape-hatch keys (sandbox backend,
	// on_delta, …) and connection_id is plumbed through the plugin
	// layer, not the upstream tool.
	upstreamInput := map[string]interface{}{}
	for k, v := range input {
		if k == "connection_id" || strings.HasPrefix(k, "__") {
			continue
		}
		upstreamInput[k] = v
	}

	result, err := client.CallTool(ctx, t.upstream, upstreamInput)
	if err != nil {
		t.plugin.recordError(connectionID, err.Error())
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	t.plugin.recordActivity(connectionID)

	// Flatten the MCP ToolResult into the map shape Nomi tools return.
	// `content` is an array of {type, text|data} entries; concatenate
	// text parts and keep the raw slice for callers that need the
	// structured form (e.g. base64 screenshot bytes).
	out := map[string]interface{}{
		"is_error": result.IsError,
	}
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
	return out, nil
}
