// Package teams implements the Microsoft Teams channel plugin via the
// Bot Framework Activity webhook (OpenClaw long-tail). One Connection =
// one Azure Bot (App ID + password). Inbound traffic arrives at
// /webhooks/com.nomi.teams/:connection_id; outbound replies use the
// Bot Connector API. Plan review uses Adaptive Card Approve/Deny.
package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
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
const PluginID = "com.nomi.teams"

// Plugin implements plugins.Plugin + WebhookReceiver + ChannelProvider +
// ToolProvider + ConnectionHealthReporter for Microsoft Teams.
type Plugin struct {
	rt            *runtime.Runtime
	connections   *db.ConnectionRepository
	bindings      *db.AssistantBindingRepository
	conversations *db.ConversationRepository
	identities    *db.ChannelIdentityRepository
	runs          *db.RunRepository
	eventBus      *events.EventBus
	secrets       secrets.Store

	mu            sync.RWMutex
	running       bool
	healthPerConn map[string]*plugins.ConnectionHealth
	planMsg       map[string]planMsgRef
	// convRefs remembers serviceUrl + conversation id for replies.
	convRefs map[string]conversationRef // key: connectionID + "\x00" + conversationID
}

type conversationRef struct {
	ServiceURL     string
	ConversationID string
}

// NewPlugin wires the Teams plugin.
func NewPlugin(
	rt *runtime.Runtime,
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	convs *db.ConversationRepository,
	idents *db.ChannelIdentityRepository,
	runs *db.RunRepository,
	eventBus *events.EventBus,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		rt:            rt,
		connections:   conns,
		bindings:      binds,
		conversations: convs,
		identities:    idents,
		runs:          runs,
		eventBus:      eventBus,
		secrets:       secretStore,
		healthPerConn: map[string]*plugins.ConnectionHealth{},
		planMsg:       map[string]planMsgRef{},
		convRefs:      map[string]conversationRef{},
	}
}

// Manifest declares the Teams plugin contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Microsoft Teams",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Microsoft Teams bot via Bot Framework Activity webhooks. Point the Azure Bot messaging endpoint at /webhooks/com.nomi.teams/:connection_id. Safe plans approve via Adaptive Card buttons.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"teams.post",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "teams",
				Description:       "Microsoft Teams chat / channel",
				SupportsThreading: true,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "teams.post_message",
				Capability:         "teams.post",
				Description:        "Post a message to a Teams conversation. Inputs: connection_id, conversation_id, text (service_url optional if the conversation was seen inbound).",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{
					Kind:        "teams_app_id",
					Key:         "webhook_secret",
					Label:       "Microsoft App ID",
					Required:    true,
					Description: "Azure Bot / App Registration Application (client) ID. Used as the Bot Framework JWT audience (stored as webhook_secret for the shared webhook router).",
				},
				{
					Kind:        "teams_app_password",
					Key:         "app_password",
					Label:       "Microsoft App Password",
					Required:    true,
					Description: "Client secret for the Azure Bot — used to obtain Bot Connector access tokens for outbound replies.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `"drop" | "reply_request_access" | "queue_approval".`,
				},
			},
			NetworkAllowlist: []string{
				"login.microsoftonline.com",
				"login.botframework.com",
				"*.trafficmanager.net",
				"smba.trafficmanager.net",
			},
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
	p.running = true
	p.mu.Unlock()

	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		return fmt.Errorf("list teams connections: %w", err)
	}
	p.mu.Lock()
	for _, conn := range conns {
		if conn.Enabled {
			p.healthPerConn[conn.ID] = &plugins.ConnectionHealth{Running: true}
		}
	}
	p.mu.Unlock()

	if p.eventBus != nil && p.rt != nil && p.runs != nil {
		go p.subscribePlanReview(ctx)
	}
	return nil
}

func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

func (p *Plugin) Channels() []plugins.Channel {
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		return nil
	}
	out := make([]plugins.Channel, 0, len(conns))
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		out = append(out, &Channel{plugin: p, connectionID: conn.ID})
	}
	return out
}

// activity is the subset of Bot Framework Activity we handle.
type activity struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Timestamp    string         `json:"timestamp"`
	ServiceURL   string         `json:"serviceUrl"`
	ChannelID    string         `json:"channelId"`
	Text         string         `json:"text"`
	Name         string         `json:"name"`
	From         channelAccount `json:"from"`
	Recipient    channelAccount `json:"recipient"`
	Conversation struct {
		ID string `json:"id"`
	} `json:"conversation"`
	Value json.RawMessage `json:"value"`
}

type channelAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReceiveWebhook parses a verified Bot Framework Activity.
func (p *Plugin) ReceiveWebhook(ctx context.Context, connectionID string, body []byte, _ map[string]string, onFire plugins.TriggerCallback) error {
	var act activity
	if err := json.Unmarshal(body, &act); err != nil {
		return fmt.Errorf("parse teams activity: %w", err)
	}
	if act.Conversation.ID != "" && act.ServiceURL != "" {
		p.rememberConversation(connectionID, act.ServiceURL, act.Conversation.ID)
	}

	switch act.Type {
	case "invoke":
		p.handleInvoke(ctx, connectionID, act)
		p.recordActivity(connectionID, time.Now().UTC())
		return nil
	case "message":
		text := strings.TrimSpace(act.Text)
		if text == "" {
			return nil
		}
		handled, err := p.handleInboundText(ctx, connectionID, act)
		if err != nil {
			slog.Error("teams: inbound text failed", "connection_id", connectionID, "error", err)
			p.recordError(connectionID, err.Error())
			return err
		}
		if handled {
			p.recordActivity(connectionID, time.Now().UTC())
			return nil
		}
		event := plugins.TriggerEvent{
			ConnectionID: connectionID,
			Kind:         "teams",
			Goal:         fmt.Sprintf("Teams message from %s: %s", displayName(act.From), text),
			Metadata: map[string]interface{}{
				"from_id":         act.From.ID,
				"from_name":       act.From.Name,
				"conversation_id": act.Conversation.ID,
				"service_url":     act.ServiceURL,
				"activity_id":     act.ID,
				"text":            text,
			},
		}
		if err := onFire(ctx, event); err != nil {
			return err
		}
		p.recordActivity(connectionID, time.Now().UTC())
		return nil
	case "conversationUpdate":
		return nil
	default:
		return nil
	}
}

func (p *Plugin) handleInboundText(ctx context.Context, connID string, act activity) (bool, error) {
	if p.rt == nil || p.bindings == nil {
		return false, nil
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		return false, nil
	}
	sender := act.From.ID
	if !p.senderAllowed(connID, assistantID, sender) {
		p.handleFirstContact(ctx, connID, act)
		return true, nil
	}

	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, act.Conversation.ID, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	goal := strings.TrimSpace(act.Text)
	if name := displayName(act.From); name != "" {
		goal = fmt.Sprintf("Teams message from %s: %s", name, goal)
	}
	_, err = p.rt.CreateRunInConversation(ctx, goal, assistantID, "teams", conversationID)
	if err != nil {
		return true, fmt.Errorf("create run: %w", err)
	}
	return true, nil
}

func displayName(a channelAccount) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

func (p *Plugin) rememberConversation(connID, serviceURL, conversationID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.convRefs[connID+"\x00"+conversationID] = conversationRef{
		ServiceURL:     strings.TrimRight(serviceURL, "/"),
		ConversationID: conversationID,
	}
}

func (p *Plugin) lookupConversation(connID, conversationID string) (conversationRef, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ref, ok := p.convRefs[connID+"\x00"+conversationID]
	return ref, ok
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

func (p *Plugin) handleFirstContact(ctx context.Context, connID string, act activity) {
	policy := domain.FirstContactDrop
	if p.connections != nil {
		conn, err := p.connections.GetByID(connID)
		if err == nil && conn != nil {
			raw, _ := conn.Config["first_contact_policy"].(string)
			if domain.FirstContactPolicy(raw).IsValid() {
				policy = domain.FirstContactPolicy(raw)
			}
		}
	}
	switch policy {
	case domain.FirstContactReplyRequestAccess:
		_ = p.replyText(ctx, connID, act.ServiceURL, act.Conversation.ID,
			"Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.")
	case domain.FirstContactQueueApproval:
		if p.identities != nil && act.From.ID != "" {
			_ = p.identities.Create(&domain.ChannelIdentity{
				PluginID:           PluginID,
				ConnectionID:       connID,
				ExternalIdentifier: act.From.ID,
				DisplayName:        displayName(act.From),
				Enabled:            false,
			})
		}
	case domain.FirstContactDrop:
		log.Printf("[teams plugin] dropped unknown sender on %s", connID)
	}
}

func (p *Plugin) recordActivity(connectionID string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.healthPerConn[connectionID]
	if !ok {
		h = &plugins.ConnectionHealth{Running: true}
		p.healthPerConn[connectionID] = h
	}
	h.LastEventAt = at
	h.LastError = ""
	h.ErrorCount = 0
}

func (p *Plugin) recordError(connectionID, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.healthPerConn[connectionID]
	if !ok {
		h = &plugins.ConnectionHealth{Running: true}
		p.healthPerConn[connectionID] = h
	}
	h.LastError = msg
	h.ErrorCount++
}

// Channel implements plugins.Channel for outbound Teams messages.
type Channel struct {
	plugin       *Plugin
	connectionID string
}

func (c *Channel) ConnectionID() string { return c.connectionID }
func (c *Channel) Kind() string         { return "teams" }

func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	if len(msg.Attachments) > 0 {
		return fmt.Errorf("teams channel: attachments not supported in v1")
	}
	ref, ok := c.plugin.lookupConversation(c.connectionID, externalConversationID)
	if !ok || ref.ServiceURL == "" {
		return fmt.Errorf("teams channel: no serviceUrl for conversation %s (need an inbound message first)", externalConversationID)
	}
	return c.plugin.replyText(ctx, c.connectionID, ref.ServiceURL, externalConversationID, msg.Text)
}
