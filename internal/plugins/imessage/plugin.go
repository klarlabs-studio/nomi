// Package imessage implements the iMessage channel plugin via a
// BlueBubbles Server sidecar on macOS (OpenClaw long-tail). One
// Connection = one BlueBubbles instance (base URL + server password).
// Polls /api/v1/message/query; plan review uses APPROVE/DENY replies.
package imessage

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
const PluginID = "com.nomi.imessage"

// Plugin implements plugins.Plugin + ChannelProvider + ToolProvider +
// ConnectionHealthReporter for iMessage via BlueBubbles.
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
	seenGUID      map[string]map[string]struct{} // connection → message guid
}

// NewPlugin wires the iMessage plugin.
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
		seenGUID:      map[string]map[string]struct{}{},
	}
}

func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "iMessage",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "iMessage via BlueBubbles Server on macOS. Paste the server base URL + password; Nomi polls for new messages. Safe plans approve via APPROVE/DENY reply.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"imessage.post",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "imessage",
				Description:       "iMessage DM / group (BlueBubbles)",
				SupportsThreading: false,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "imessage.post_message",
				Capability:         "imessage.post",
				Description:        "Send an iMessage. Inputs: connection_id, chat_guid (e.g. iMessage;-;+15551234567), text",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{{
				Kind:        "bluebubbles_password",
				Key:         "password",
				Label:       "BlueBubbles server password",
				Required:    true,
				Description: "Password from BlueBubbles Server → Settings → API.",
			}},
			ConfigSchema: map[string]plugins.ConfigField{
				"api_base_url": {
					Type: "string", Label: "BlueBubbles base URL",
					Required:    true,
					Description: "e.g. http://127.0.0.1:1234 or your Cloudflare tunnel URL",
				},
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `"drop" | "reply_request_access" | "queue_approval".`,
				},
			},
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
		return fmt.Errorf("list imessage connections: %w", err)
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
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		err := fmt.Errorf("connection %s missing api_base_url", conn.ID)
		log.Printf("[imessage plugin] %v", err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	password, err := p.resolveSecret(conn, "password")
	if err != nil {
		log.Printf("[imessage plugin] %v", err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	cli := newClient(baseURL, password)
	if err := cli.ping(ctx); err != nil {
		log.Printf("[imessage plugin] ping %s failed", conn.ID)
		p.recordConnectionError(conn.ID, err)
		// Still start polling — server may come up later.
	}
	p.mu.Lock()
	p.clients[conn.ID] = cli
	p.seenGUID[conn.ID] = map[string]struct{}{}
	p.mu.Unlock()
	p.recordConnectionSuccess(conn.ID)
	go p.pollLoop(ctx, conn.ID, cli)
}

func (p *Plugin) resolveSecret(conn *domain.Connection, key string) (string, error) {
	ref, ok := conn.CredentialRefs[key]
	if !ok || ref == "" {
		return "", fmt.Errorf("connection %s missing %s credential", conn.ID, key)
	}
	if p.secrets == nil {
		return ref, nil
	}
	return secrets.Resolve(p.secrets, ref)
}

func (p *Plugin) pollLoop(ctx context.Context, connID string, cli *Client) {
	// Prime seen set without creating runs for historical messages.
	if msgs, err := cli.queryMessages(ctx, 25); err == nil {
		p.mu.Lock()
		seen := p.seenGUID[connID]
		if seen == nil {
			seen = map[string]struct{}{}
			p.seenGUID[connID] = seen
		}
		for _, m := range msgs {
			if m.GUID != "" {
				seen[m.GUID] = struct{}{}
			}
		}
		p.mu.Unlock()
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollOnce(ctx, connID, cli)
		}
	}
}

func (p *Plugin) pollOnce(ctx context.Context, connID string, cli *Client) {
	msgs, err := cli.queryMessages(ctx, 25)
	if err != nil {
		p.recordConnectionError(connID, err)
		return
	}
	p.recordConnectionSuccess(connID)
	// Process oldest-first so conversation order is natural.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.GUID == "" || m.IsFromMe {
			continue
		}
		p.mu.Lock()
		seen := p.seenGUID[connID]
		if seen == nil {
			seen = map[string]struct{}{}
			p.seenGUID[connID] = seen
		}
		if _, ok := seen[m.GUID]; ok {
			p.mu.Unlock()
			continue
		}
		seen[m.GUID] = struct{}{}
		// Cap memory.
		if len(seen) > 500 {
			p.seenGUID[connID] = map[string]struct{}{m.GUID: {}}
		}
		p.mu.Unlock()

		text := strings.TrimSpace(m.Text)
		chatGUID := chatGUIDFromMessage(m)
		sender := senderFromMessage(m)
		if text == "" || chatGUID == "" {
			continue
		}
		if p.tryHandlePlanReply(ctx, connID, sender, chatGUID, text) {
			continue
		}
		p.onMessage(ctx, connID, cli, sender, chatGUID, text)
	}
}

func (p *Plugin) onMessage(ctx context.Context, connID string, cli *Client, sender, chatGUID, body string) {
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[imessage plugin] no channel binding on %s", connID)
		return
	}
	if !p.senderAllowed(connID, assistantID, sender) {
		p.handleFirstContact(ctx, connID, cli, sender, chatGUID)
		return
	}
	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, chatGUID, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}
	goal := body
	if sender != "" {
		goal = fmt.Sprintf("iMessage from %s: %s", sender, body)
	}
	_, err = p.rt.CreateRunInConversation(ctx, goal, assistantID, "imessage", conversationID)
	if err != nil {
		log.Printf("[imessage plugin] create run failed")
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

func (p *Plugin) handleFirstContact(ctx context.Context, connID string, cli *Client, sender, chatGUID string) {
	switch p.firstContactPolicy(connID) {
	case domain.FirstContactReplyRequestAccess:
		_ = cli.sendText(ctx, chatGUID, "Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.")
	case domain.FirstContactQueueApproval:
		if p.identities != nil && sender != "" {
			_ = p.identities.Create(&domain.ChannelIdentity{
				PluginID:           PluginID,
				ConnectionID:       connID,
				ExternalIdentifier: sender,
				DisplayName:        sender,
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

// Channel implements plugins.Channel for outbound iMessage.
type Channel struct {
	connectionID string
	client       *Client
}

func (c *Channel) ConnectionID() string { return c.connectionID }
func (c *Channel) Kind() string         { return "imessage" }

func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	if len(msg.Attachments) > 0 {
		return fmt.Errorf("imessage channel: attachments not supported in v1")
	}
	return c.client.sendText(ctx, externalConversationID, msg.Text)
}
