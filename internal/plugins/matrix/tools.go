package matrix

import (
	"context"
	"fmt"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/tools"
)

func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&postMessageTool{plugin: p},
	}
}

type postMessageTool struct {
	plugin *Plugin
}

func (t *postMessageTool) Name() string       { return "matrix.post_message" }
func (t *postMessageTool) Capability() string { return "matrix.post" }

func (t *postMessageTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("matrix.post_message: connection_id is required")
	}
	roomID, _ := input["room_id"].(string)
	if roomID == "" {
		return nil, fmt.Errorf("matrix.post_message: room_id is required")
	}
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("matrix.post_message: text is required")
	}

	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("matrix.post_message: binding check failed: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	t.plugin.mu.RLock()
	cli, ok := t.plugin.clients[connectionID]
	t.plugin.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("matrix.post_message: connection %s is not running", connectionID)
	}

	eventID, err := cli.sendText(ctx, roomID, text)
	if err != nil {
		return nil, fmt.Errorf("matrix.post_message: %w", err)
	}
	return map[string]interface{}{
		"room_id":  roomID,
		"event_id": eventID,
	}, nil
}

var _ tools.Tool = (*postMessageTool)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ plugins.ChannelProvider = (*Plugin)(nil)
