package discord

import (
	"context"
	"fmt"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/tools"
)

// Discord ToolProvider: assistants can post messages to a Discord
// channel as part of a plan. Same binding-enforcement pattern as Email
// and Slack.

func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&postMessageTool{plugin: p},
	}
}

type postMessageTool struct {
	plugin *Plugin
}

func (t *postMessageTool) Name() string       { return "discord.post_message" }
func (t *postMessageTool) Capability() string { return "discord.post" }

func (t *postMessageTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("discord.post_message: connection_id is required")
	}
	channelID, _ := input["channel_id"].(string)
	if channelID == "" {
		return nil, fmt.Errorf("discord.post_message: channel_id is required")
	}
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("discord.post_message: text is required")
	}

	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("discord.post_message: binding check failed: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	t.plugin.mu.RLock()
	sess, ok := t.plugin.sessions[connectionID]
	t.plugin.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("discord.post_message: connection %s is not running", connectionID)
	}

	msg, err := sess.ChannelMessageSend(channelID, text)
	if err != nil {
		return nil, fmt.Errorf("discord.post_message: %w", err)
	}
	_ = ctx // session calls are synchronous; ctx kept for signature consistency
	return map[string]interface{}{
		"channel_id": msg.ChannelID,
		"message_id": msg.ID,
	}, nil
}

var _ tools.Tool = (*postMessageTool)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
