package slack

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/tools"
)

// Slack ToolProvider. Assistants can proactively post to Slack channels
// as part of a plan. Same binding-enforcement pattern as Email: every
// tool call takes a connection_id, and the runtime rejects calls to a
// connection the assistant isn't bound to under role=tool.

// Tools implements plugins.ToolProvider.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&postMessageTool{plugin: p},
	}
}

type postMessageTool struct {
	plugin *Plugin
}

func (t *postMessageTool) Name() string       { return "slack.post_message" }
func (t *postMessageTool) Capability() string { return "slack.post" }

func (t *postMessageTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("slack.post_message: connection_id is required")
	}
	channel, _ := input["channel"].(string)
	if channel == "" {
		return nil, fmt.Errorf("slack.post_message: channel is required (channel id or #channel-name)")
	}
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("slack.post_message: text is required")
	}

	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("slack.post_message: binding check failed: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	conn, err := t.plugin.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("slack.post_message: %w", err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("slack.post_message: connection %s is disabled", connectionID)
	}
	client, err := t.plugin.resolveClient(conn)
	if err != nil {
		return nil, fmt.Errorf("slack.post_message: %w", err)
	}

	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	// Optional thread_ts for replying inside an existing thread.
	if threadTS, _ := input["thread_ts"].(string); threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	channelID, ts, err := client.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return nil, fmt.Errorf("slack.post_message: %w", err)
	}
	return map[string]interface{}{
		"channel": channelID,
		"ts":      ts,
	}, nil
}

// Compile-time interface guard.
var _ tools.Tool = (*postMessageTool)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
