package whatsapp

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/plugins"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// Tools implements plugins.ToolProvider — exposes whatsapp.send_message
// so assistants can reply on the same WhatsApp conversation that
// triggered the run.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&sendMessageTool{plugin: p, http: &http.Client{Timeout: 15 * time.Second}},
	}
}

type sendMessageTool struct {
	plugin *Plugin
	http   *http.Client
}

func (t *sendMessageTool) Name() string       { return "whatsapp.send_message" }
func (t *sendMessageTool) Capability() string { return "whatsapp.send" }

func (t *sendMessageTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("whatsapp.send_message: connection_id is required")
	}
	to, _ := input["to"].(string)
	if to == "" {
		return nil, fmt.Errorf("whatsapp.send_message: to (E.164 phone) is required")
	}
	text, _ := input["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("whatsapp.send_message: text is required")
	}

	assistantID, _ := input["__assistant_id"].(string)
	if assistantID != "" && t.plugin.bindings != nil {
		ok, err := t.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("whatsapp.send_message: binding check: %w", err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}

	conn, err := t.plugin.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("whatsapp.send_message: %w", err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("whatsapp.send_message: connection %s is disabled", connectionID)
	}

	phoneNumberID, _ := conn.Config["phone_number_id"].(string)
	if phoneNumberID == "" {
		return nil, fmt.Errorf("whatsapp.send_message: connection %s missing phone_number_id config", connectionID)
	}

	tokenRef, ok := conn.CredentialRefs["access_token"]
	if !ok || tokenRef == "" {
		return nil, fmt.Errorf("whatsapp.send_message: connection %s missing access_token credential", connectionID)
	}
	accessToken, err := secrets.Resolve(t.plugin.secrets, tokenRef)
	if err != nil {
		return nil, fmt.Errorf("whatsapp.send_message: resolve token: %w", err)
	}

	resp, err := SendText(ctx, t.http, SendTextOptions{
		PhoneNumberID: phoneNumberID,
		AccessToken:   accessToken,
		To:            to,
		Body:          text,
	})
	if err != nil {
		return nil, err
	}

	out := map[string]interface{}{
		"to": to,
	}
	if len(resp.Messages) > 0 {
		out["message_id"] = resp.Messages[0].ID
	}
	if len(resp.Contacts) > 0 {
		out["wa_id"] = resp.Contacts[0].WAID
	}
	return out, nil
}

// Compile-time interface guards.
var _ tools.Tool = (*sendMessageTool)(nil)
var _ plugins.ToolProvider = (*Plugin)(nil)
