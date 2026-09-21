// Package matrix implements the Matrix channel plugin (Client-Server
// API v3). One Connection = one Matrix account (user or appservice
// bot) identified by homeserver URL + access token. Opens a long-poll
// /sync loop per connection; inbound room messages create runs, and
// plan_review prompts accept APPROVE/DENY replies or ✅/❌ reactions —
// OpenClaw long-tail wedge without skipping Nomi's plan gate.
package matrix

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
const PluginID = "com.nomi.matrix"

// Plugin implements plugins.Plugin + ChannelProvider + ToolProvider +
// ConnectionHealthReporter for Matrix.
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
	clients       map[string]*Client // connection_id → client
	healthPerConn map[string]*plugins.ConnectionHealth
	planMsg       map[string]planMsgRef // run_id → prompt ref
}

// NewPlugin wires the Matrix plugin.
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

// Manifest declares the plugin contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Matrix",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Matrix homeserver bot via Client-Server API. DM or room messages create runs; safe plans approve via APPROVE/DENY reply or ✅/❌ reaction.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"matrix.post",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "matrix",
				Description:       "Matrix DM / room",
				SupportsThreading: true,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "matrix.post_message",
				Capability:         "matrix.post",
				Description:        "Post a message to a Matrix room. Inputs: connection_id, room_id, text",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{{
				Kind:        "matrix_access_token",
				Key:         "access_token",
				Label:       "Access Token",
				Required:    true,
				Description: "Matrix access token for a user or bot account (Element: Settings → Help & About → Access Token).",
			}},
			ConfigSchema: map[string]plugins.ConfigField{
				"homeserver_url": {
					Type: "string", Label: "Homeserver URL",
					Required:    true,
					Description: "Base URL of the Matrix homeserver, e.g. https://matrix.org",
				},
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `"drop" | "reply_request_access" | "queue_approval".`,
				},
			},
			// Homeserver host is per-connection; empty allowlist means
			// no fixed host gate at the plugin manifest layer (same as
			// Email / Scout).
			NetworkAllowlist: nil,
		},
	}
}

// Configure is a no-op; state lives in plugin_connections.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Status returns plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// ConnectionHealth implements plugins.ConnectionHealthReporter.
func (p *Plugin) ConnectionHealth(connectionID string) (plugins.ConnectionHealth, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.healthPerConn[connectionID]
	if !ok || h == nil {
		return plugins.ConnectionHealth{}, false
	}
	return *h, true
}

// Start opens a /sync loop per enabled connection.
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
		return fmt.Errorf("list matrix connections: %w", err)
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

// Stop cancels sync loops.
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

// Channels returns one Channel per running connection.
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
	token, err := p.resolveSecret(conn, "access_token")
	if err != nil {
		log.Printf("[matrix plugin] %v; skipping %s", err, conn.ID)
		p.recordConnectionError(conn.ID, err)
		return
	}
	homeserver, _ := conn.Config["homeserver_url"].(string)
	homeserver = strings.TrimSpace(homeserver)
	if homeserver == "" {
		err := fmt.Errorf("connection %s missing homeserver_url", conn.ID)
		log.Printf("[matrix plugin] %v", err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	cli := newClient(homeserver, token)
	if _, err := cli.whoami(ctx); err != nil {
		log.Printf("[matrix plugin] whoami %s: %v", conn.ID, err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	p.mu.Lock()
	p.clients[conn.ID] = cli
	p.mu.Unlock()
	p.recordConnectionSuccess(conn.ID)
	go p.syncLoop(ctx, conn.ID, cli)
}

func (p *Plugin) syncLoop(ctx context.Context, connID string, cli *Client) {
	var since string
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		resp, err := cli.sync(ctx, since, 30000)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[matrix plugin] sync %s: %v", connID, err)
			p.recordConnectionError(connID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		p.recordConnectionSuccess(connID)
		if !first {
			p.handleSync(ctx, connID, cli, resp)
		} else {
			// First sync establishes next_batch without replaying
			// historical timeline as new runs.
			for roomID := range resp.Rooms.Invite {
				if err := cli.joinRoom(ctx, roomID); err != nil {
					log.Printf("[matrix plugin] join invite %s: %v", roomID, err)
				}
			}
			first = false
		}
		if resp.NextBatch != "" {
			since = resp.NextBatch
		}
	}
}

func (p *Plugin) handleSync(ctx context.Context, connID string, cli *Client, resp *syncResponse) {
	for roomID := range resp.Rooms.Invite {
		if err := cli.joinRoom(ctx, roomID); err != nil {
			log.Printf("[matrix plugin] join invite %s: %v", roomID, err)
		}
	}
	for roomID, room := range resp.Rooms.Join {
		for _, ev := range room.Timeline.Events {
			p.handleEvent(ctx, connID, cli, roomID, ev)
		}
	}
}

func (p *Plugin) handleEvent(ctx context.Context, connID string, cli *Client, roomID string, ev rawEvent) {
	if cli.UserID != "" && ev.Sender == cli.UserID {
		return
	}
	switch ev.Type {
	case "m.reaction":
		p.handleReaction(ctx, connID, roomID, ev)
	case "m.room.message":
		body, ok := parseTextBody(ev.Content)
		if !ok {
			return
		}
		if p.tryHandlePlanReply(ctx, connID, roomID, ev.Sender, body) {
			return
		}
		p.onMessage(ctx, connID, cli, roomID, ev.Sender, body)
	}
}

func (p *Plugin) onMessage(ctx context.Context, connID string, cli *Client, roomID, sender, body string) {
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[matrix plugin] %v; dropping message on %s", err, connID)
		return
	}
	if !p.senderAllowed(connID, assistantID, sender) {
		p.handleFirstContact(ctx, connID, cli, roomID, sender, p.firstContactPolicy(connID))
		return
	}

	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, roomID, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	_, err = p.rt.CreateRunInConversation(ctx, body, assistantID, "matrix", conversationID)
	if err != nil {
		log.Printf("[matrix plugin] create run: %v", err)
	}
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

func (p *Plugin) handleFirstContact(ctx context.Context, connID string, cli *Client, roomID, sender string, policy domain.FirstContactPolicy) {
	switch policy {
	case domain.FirstContactReplyRequestAccess:
		if cli != nil && roomID != "" {
			_, _ = cli.sendText(ctx, roomID,
				"Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.")
		}
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

// Channel implements plugins.Channel for outbound Matrix messages.
type Channel struct {
	connectionID string
	client       *Client
}

func (c *Channel) ConnectionID() string { return c.connectionID }
func (c *Channel) Kind() string         { return "matrix" }

func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	if len(msg.Attachments) > 0 {
		return fmt.Errorf("matrix channel: attachments not supported in v1")
	}
	_, err := c.client.sendText(ctx, externalConversationID, msg.Text)
	return err
}
