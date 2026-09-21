package signal

import (
	"context"
	"fmt"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/tools"
)

func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{&postMessageTool{plugin: p}}
}

type postMessageTool struct {
	plugin *Plugin
}

func (t *postMessageTool) Name() string       { return "signal.post_message" }
func (t *postMessageTool) Capability() string { return "signal.post" }

func (t *postMessageTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("signal.post_message: connection_id is required")
	}
	recipient, _ := input["recipient"].(string)
	if recipient == "" {
		return nil, fmt.Errorf("signal.post_message: recipient is required")
	}
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("signal.post_message: text is required")
	}

	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("signal.post_message: binding check failed: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	t.plugin.mu.RLock()
	cli, ok := t.plugin.clients[connectionID]
	t.plugin.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("signal.post_message: connection %s is not running", connectionID)
	}
	if err := cli.sendText(ctx, recipient, text); err != nil {
		return nil, fmt.Errorf("signal.post_message: %w", err)
	}
	return map[string]interface{}{"recipient": recipient}, nil
}

var _ tools.Tool = (*postMessageTool)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ plugins.ChannelProvider = (*Plugin)(nil)
