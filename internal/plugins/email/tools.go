package email

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/email/transport"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// Email ToolProvider. Tools are plugin-provided but Connection-aware: each
// call takes a connection_id input so a single assistant can send email
// from multiple accounts depending on context. The runtime's
// connection_not_bound hard wall (plugins.ErrConnectionNotBound) gates
// this at the tool layer — an assistant with no email binding is refused
// before the Gmail API is ever touched.

// Tools implements plugins.ToolProvider so the Email plugin can contribute
// email.send into the shared tools.Registry.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&sendTool{plugin: p},
	}
}

// sendTool implements email.send: assistant composes an outbound message
// and the runtime dispatches it through the Connection the assistant
// is bound to. connection_id is a required input so the LLM picks the
// right account when the assistant has multiple email bindings.
type sendTool struct {
	plugin *Plugin
}

func (t *sendTool) Name() string { return "email.send" }

// Capability is plugin-specific so each channel's outbound has its own
// permission knob. The manifest's broader network.outgoing entry is the
// declared ceiling; this is the gated operation.
func (t *sendTool) Capability() string { return "email.send" }

func (t *sendTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("email.send: connection_id is required")
	}
	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("email.send: binding check failed: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	to := coerceStringSlice(input["to"])
	if len(to) == 0 {
		return nil, fmt.Errorf("email.send: 'to' is required (string or array of strings)")
	}
	subject, _ := input["subject"].(string)
	body, _ := input["body"].(string)
	if body == "" {
		return nil, fmt.Errorf("email.send: 'body' is required")
	}
	if subject == "" {
		subject = "(no subject)"
	}
	replyTo, _ := input["in_reply_to"].(string)
	references := coerceStringSlice(input["references"])

	conn, err := t.plugin.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("email.send: %w", err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("email.send: connection %s is disabled", connectionID)
	}
	cfg, err := t.plugin.buildTransportConfig(conn)
	if err != nil {
		return nil, fmt.Errorf("email.send: %w", err)
	}
	if err := transport.SendEmail(cfg, to, subject, body, replyTo, references); err != nil {
		return nil, fmt.Errorf("email.send: %w", err)
	}
	return map[string]interface{}{
		"status": "sent",
		"to":     strings.Join(to, ", "),
	}, nil
}

// coerceStringSlice accepts either a single string or []interface{} and
// normalizes to []string. The planner emits both shapes depending on
// whether it has a single-recipient or multi-recipient intent.
func coerceStringSlice(v interface{}) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Silence unused-import warning if json is not otherwise used.
var _ = json.RawMessage{}

// Ensure sendTool satisfies tools.Tool at compile time.
var _ tools.Tool = (*sendTool)(nil)

// Ensure Plugin satisfies plugins.ToolProvider.
var _ plugins.ToolProvider = (*Plugin)(nil)

// _ = db avoids unused-import when future tool variants stop needing it.
var _ = (*db.ConnectionRepository)(nil)
