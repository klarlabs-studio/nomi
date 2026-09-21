// Package signal implements the Signal channel plugin via a
// signal-cli-rest-api sidecar (ADR 0001 stretch). One Connection = one
// registered Signal number + REST base URL. Opens a long-poll /v1/receive
// loop; plan review uses APPROVE/DENY replies (and ✅/❌ reactions when
// present) — OpenClaw long-tail without skipping Nomi's plan gate.
package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.signal"

// Plugin implements plugins.Plugin + ChannelProvider + ToolProvider +
// ConnectionHealthReporter for Signal.
type Plugin struct {
	rt            *runtime.Runtime
	connections   *db.ConnectionRepository
	bindings      *db.AssistantBindingRepository
	conversations *db.ConversationRepository
	identities    *db.ChannelIdentityRepository
	runs          *db.RunRepository
	secrets       secrets.Store
	eventBus      *events.EventBus

	mu            sync.RWMutex
	running       bool
	cancel        context.CancelFunc
	clients       map[string]*Client
	healthPerConn map[string]*plugins.ConnectionHealth
	planMsg       map[string]planMsgRef
}

// NewPlugin wires the Signal plugin.
func NewPlugin(
	rt *runtime.Runtime,
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	convs *db.ConversationRepository,
	idents *db.ChannelIdentityRepository,
	runs *db.RunRepository,
	secrets secrets.Store,
	eventBus *events.EventBus,
) *Plugin {
	return &Plugin{
		rt:            rt,
		connections:   conns,
		bindings:      binds,
		conversations: convs,
		identities:    idents,
		runs:          runs,
		secrets:       secrets,
		eventBus:      eventBus,
		clients:       map[string]*Client{},
		healthPerConn: map[string]*plugins.ConnectionHealth{},
		planMsg:       map[string]planMsgRef{},
	}
}

func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Signal",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Signal messaging via signal-cli-rest-api. Run the sidecar locally, paste its base URL + registered E.164 number. Safe plans approve via APPROVE/DENY reply.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"signal.post",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "signal",
				Description:       "Signal DM / group",
				SupportsThreading: false,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "signal.post_message",
				Capability:         "signal.post",
				Description:        "Send a Signal text message. Inputs: connection_id, recipient (E.164 or group:<id>), text",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{{
				Kind:        "signal_api_token",
				Key:         "api_token",
				Label:       "API Token (optional)",
				Required:    false,
				Description: "Bearer token if your signal-cli-rest-api instance requires auth.",
			}},
			ConfigSchema: map[string]plugins.ConfigField{
				"api_base_url": {
					Type: "string", Label: "REST API base URL",
					Required:    true,
					Description: "signal-cli-rest-api base URL, e.g. http://127.0.0.1:8080",
				},
				"account": {
					Type: "string", Label: "Signal account (E.164)",
					Required:    true,
					Description: "Registered Signal number in international format, e.g. +15551234567",
				},
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `"drop" | "reply_request_access" | "queue_approval".`,
				},
			},
			// Sidecar host is per-connection (often localhost).
			NetworkAllowlist: nil,
		},
	}
}

func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

func (p *Plugin) ConnectionHealth(connectionID string) (plugins.ConnectionHealth, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.healthPerConn[connectionID]
	if !ok || h == nil {
		return plugins.ConnectionHealth{}, false
	}
	return *h, true
}

func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.running = true
	p.mu.Unlock()

	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		cancel()
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		return fmt.Errorf("list signal connections: %w", err)
	}
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		p.startConnection(runCtx, conn)
	}
	if p.eventBus != nil && p.rt != nil && p.runs != nil {
		go p.subscribePlanReview(runCtx)
	}
	return nil
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.clients = map[string]*Client{}
	p.running = false
	return nil
}

func (p *Plugin) Channels() []plugins.Channel {
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		return nil
	}
	out := make([]plugins.Channel, 0, len(conns))
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		cli, ok := p.clients[conn.ID]
		if !ok {
			continue
		}
		out = append(out, &Channel{connectionID: conn.ID, client: cli})
	}
	return out
}

func (p *Plugin) startConnection(ctx context.Context, conn *domain.Connection) {
	baseURL, _ := conn.Config["api_base_url"].(string)
	account, _ := conn.Config["account"].(string)
	baseURL = strings.TrimSpace(baseURL)
	account = strings.TrimSpace(account)
	if baseURL == "" || account == "" {
		err := fmt.Errorf("connection %s missing api_base_url or account", conn.ID)
		log.Printf("[signal plugin] %v", err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	token := ""
	if ref, ok := conn.CredentialRefs["api_token"]; ok && ref != "" {
		var err error
		token, err = p.resolveSecret(conn, "api_token")
		if err != nil {
			log.Printf("[signal plugin] %v; continuing without token", err)
			token = ""
		}
	}
	cli := newClient(baseURL, account, token)
	p.mu.Lock()
	p.clients[conn.ID] = cli
	p.mu.Unlock()
	p.recordConnectionSuccess(conn.ID)
	go p.receiveLoop(ctx, conn.ID, cli)
}

func (p *Plugin) resolveSecret(conn *domain.Connection, key string) (string, error) {
	ref, ok := conn.CredentialRefs[key]
	if !ok || ref == "" {
		return "", fmt.Errorf("connection %s missing %s", conn.ID, key)
	}
	if p.secrets == nil {
		return ref, nil
	}
	return secrets.Resolve(p.secrets, ref)
}

func (p *Plugin) receiveLoop(ctx context.Context, connID string, cli *Client) {
	for {
		if ctx.Err() != nil {
			return
		}
		msgs, err := cli.receive(ctx, 30)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[signal plugin] receive %s failed", connID)
			p.recordConnectionError(connID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		p.recordConnectionSuccess(connID)
		for _, ev := range msgs {
			p.handleEnvelope(ctx, connID, cli, ev)
		}
	}
}

func (p *Plugin) handleEnvelope(ctx context.Context, connID string, cli *Client, ev receiveEnvelope) {
	if ev.Envelope == nil || ev.Envelope.DataMessage == nil {
		return
	}
	dm := ev.Envelope.DataMessage
	sender := senderFromEnvelope(ev)
	convKey := conversationFromEnvelope(ev)
	if sender == "" || convKey == "" {
		return
	}
	// Ignore our own account echoes when source matches account.
	if sender == cli.Account {
		return
	}

	if dm.Reaction != nil && dm.Reaction.Emoji != "" {
		p.handleReaction(ctx, connID, sender, convKey, dm.Reaction.Emoji)
		return
	}
	body := strings.TrimSpace(dm.Message)
	if body == "" {
		return
	}
	if p.tryHandlePlanReply(ctx, connID, sender, convKey, body) {
		return
	}
	p.onMessage(ctx, connID, cli, sender, convKey, body, ev.Envelope.SourceName)
}

func (p *Plugin) onMessage(ctx context.Context, connID string, cli *Client, sender, convKey, body, display string) {
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[signal plugin] no channel binding on %s", connID)
		return
	}
	if !p.senderAllowed(connID, assistantID, sender) {
		p.handleFirstContact(ctx, connID, cli, sender, display)
		return
	}
	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, convKey, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}
	goal := body
	if display != "" {
		goal = fmt.Sprintf("Signal message from %s (%s): %s", display, sender, body)
	}
	_, err = p.rt.CreateRunInConversation(ctx, goal, assistantID, "signal", conversationID)
	if err != nil {
		log.Printf("[signal plugin] create run failed")
	}
}

func (p *Plugin) senderAllowed(connID, assistantID, userID string) bool {
	if p.identities == nil || userID == "" {
		return true
	}
	existing, err := p.identities.ListByConnection(connID)
	if err != nil || len(existing) == 0 {
		return true
	}
	ok, _ := p.identities.IsAllowed(PluginID, connID, userID, assistantID)
	return ok
}

func (p *Plugin) firstContactPolicy(connID string) domain.FirstContactPolicy {
	if p.connections == nil {
		return domain.FirstContactDrop
	}
	conn, err := p.connections.GetByID(connID)
	if err != nil || conn == nil {
		return domain.FirstContactDrop
	}
	raw, _ := conn.Config["first_contact_policy"].(string)
	policy := domain.FirstContactPolicy(raw)
	if !policy.IsValid() {
		return domain.FirstContactDrop
	}
	return policy
}

func (p *Plugin) handleFirstContact(ctx context.Context, connID string, cli *Client, sender, display string) {
	switch p.firstContactPolicy(connID) {
	case domain.FirstContactReplyRequestAccess:
		_ = cli.sendText(ctx, sender, "Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.")
	case domain.FirstContactQueueApproval:
		if p.identities != nil && sender != "" {
			_ = p.identities.Create(&domain.ChannelIdentity{
				PluginID:           PluginID,
				ConnectionID:       connID,
				ExternalIdentifier: sender,
				DisplayName:        display,
				Enabled:            false,
			})
		}
	case domain.FirstContactDrop:
	}
}

func (p *Plugin) resolveChannelAssistant(connID string) (string, error) {
	binds, err := p.bindings.ListByConnection(connID)
	if err != nil {
		return "", err
	}
	var fallback *domain.AssistantConnectionBinding
	for _, b := range binds {
		if !b.Enabled || b.Role != domain.BindingRoleChannel {
			continue
		}
		if b.IsPrimary {
			return b.AssistantID, nil
		}
		if fallback == nil {
			fallback = b
		}
	}
	if fallback != nil {
		return fallback.AssistantID, nil
	}
	return "", fmt.Errorf("no channel-role binding for connection %s", connID)
}

func (p *Plugin) recordConnectionSuccess(connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.healthPerConn[connID]
	if !ok {
		h = &plugins.ConnectionHealth{}
		p.healthPerConn[connID] = h
	}
	h.Running = true
	h.LastEventAt = time.Now().UTC()
	h.LastError = ""
	h.ErrorCount = 0
}

func (p *Plugin) recordConnectionError(connID string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.healthPerConn[connID]
	if !ok {
		h = &plugins.ConnectionHealth{}
		p.healthPerConn[connID] = h
	}
	h.LastError = err.Error()
	h.ErrorCount++
}

// Channel implements plugins.Channel for outbound Signal messages.
type Channel struct {
	connectionID string
	client       *Client
}

func (c *Channel) ConnectionID() string { return c.connectionID }
func (c *Channel) Kind() string         { return "signal" }

func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	if len(msg.Attachments) > 0 {
		return fmt.Errorf("signal channel: attachments not supported in v1")
	}
	return c.client.sendText(ctx, externalConversationID, msg.Text)
}
