package teams

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

func (t *postMessageTool) Name() string       { return "teams.post_message" }
func (t *postMessageTool) Capability() string { return "teams.post" }

func (t *postMessageTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("teams.post_message: connection_id is required")
	}
	conversationID, _ := input["conversation_id"].(string)
	if conversationID == "" {
		return nil, fmt.Errorf("teams.post_message: conversation_id is required")
	}
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("teams.post_message: text is required")
	}
	serviceURL, _ := input["service_url"].(string)

	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("teams.post_message: binding check failed: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	if serviceURL == "" {
		ref, ok := t.plugin.lookupConversation(connectionID, conversationID)
		if !ok || ref.ServiceURL == "" {
			return nil, fmt.Errorf("teams.post_message: service_url required (or prior inbound message for this conversation)")
		}
		serviceURL = ref.ServiceURL
	} else {
		t.plugin.rememberConversation(connectionID, serviceURL, conversationID)
	}

	if err := t.plugin.replyText(ctx, connectionID, serviceURL, conversationID, text); err != nil {
		return nil, fmt.Errorf("teams.post_message: %w", err)
	}
	return map[string]interface{}{
		"conversation_id": conversationID,
		"service_url":     serviceURL,
	}, nil
}

var _ tools.Tool = (*postMessageTool)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
var _ plugins.ChannelProvider = (*Plugin)(nil)
var _ plugins.WebhookReceiver = (*Plugin)(nil)
