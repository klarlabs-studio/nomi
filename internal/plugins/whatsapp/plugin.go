// Package whatsapp implements the WhatsApp Business Cloud API channel
// plugin (roady #123). One Connection = one phone number ID + access
// token pair issued through Meta's Business Manager. Inbound traffic
// arrives via webhooks (the webhooks/router.go path); outbound replies
// go through the Graph API client in send.go.
//
// Text inbound prefers channel-role binding + Conversation linking so
// plan review can reply in-thread with interactive Approve/Deny
// buttons. Trigger-only setups still fall back to TriggerEvent →
// CreateRunFromSource. Media (images, audio) and message status
// callbacks (delivered/read) remain additive follow-ups.
package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginID is the stable reverse-DNS identifier for this plugin.
const PluginID = "com.nomi.whatsapp"

// Plugin implements plugins.Plugin + WebhookReceiver + ConnectionHealthReporter
// for the WhatsApp Cloud API.
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
	// planMsg tracks that a plan-review interactive prompt was sent for
	// a run so clear signals can send a follow-up status (Cloud API has
	// no edit-in-place for interactive messages).
	planMsg map[string]planMsgRef
}

// NewPlugin wires the WhatsApp plugin.
//
// runs enables plan-review interactive buttons when a conversation-
// linked run reaches plan_review. Pass nil to skip that integration.
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
	}
}

// Manifest declares the WhatsApp plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "WhatsApp",
		Version:     "0.2.0",
		Author:      "Nomi",
		Description: "WhatsApp Business Cloud API integration. Inbound messages route to the bound assistant; the assistant can reply through the whatsapp.send_message tool. Safe plans can be approved or denied via in-chat buttons.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"whatsapp.send",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "whatsapp",
				Description:       "WhatsApp message (Cloud API)",
				SupportsThreading: false,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "whatsapp.send_message",
				Capability:         "whatsapp.send",
				Description:        "Send a WhatsApp text message. Inputs: connection_id, to (E.164 phone), text.",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{
					Kind:        "whatsapp_access_token",
					Key:         "access_token",
					Label:       "Cloud API Access Token",
					Required:    true,
					Description: "System User access token from Meta Business Manager with whatsapp_business_messaging scope.",
				},
				{
					Kind:        "whatsapp_app_secret",
					Key:         "webhook_secret",
					Label:       "App Secret",
					Required:    true,
					Description: "Meta App Secret used to verify the X-Hub-Signature-256 header on inbound webhooks.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				"phone_number_id": {
					Type: "string", Label: "Phone Number ID",
					Required:    true,
					Description: "The phone_number_id from your Meta WhatsApp Business Account.",
				},
				"first_contact_policy": {
					Type: "enum", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: "How to handle WhatsApp senders not in the identity allowlist.",
					Options: []plugins.ConfigOption{
						{Value: "drop", Label: "Drop silently"},
						{Value: "queue_approval", Label: "Queue for approval"},
					},
				},
			},
			NetworkAllowlist: []string{"graph.facebook.com"},
		},
	}
}

// Configure is a no-op; per-connection state lives in plugin_connections.
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

// Start primes per-connection health structs and (when wired) subscribes
// to plan.proposed for interactive Approve/Deny prompts. WhatsApp uses
// inbound webhooks rather than a long-lived socket so there's nothing
// else to spin up here.
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
		return fmt.Errorf("list whatsapp connections: %w", err)
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

// Stop marks the plugin not running. There are no goroutines or sockets
// to tear down — inbound stops automatically when the daemon's HTTP
// server shuts down; the plan-review subscriber exits on ctx cancel.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

// ---------------------------------------------------------------------------
// Webhook receive path
// ---------------------------------------------------------------------------

// webhookPayload models the subset of the WhatsApp Cloud API webhook
// envelope the plugin acts on. Fields the plugin doesn't use are kept
// out of the struct so JSON unknown-field tolerance is implicit.
type webhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string `json:"id"`
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				MessagingProduct string `json:"messaging_product"`
				Metadata         struct {
					DisplayPhoneNumber string `json:"display_phone_number"`
					PhoneNumberID      string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
					WAID string `json:"wa_id"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
					Interactive *struct {
						Type        string `json:"type"`
						ButtonReply *struct {
							ID    string `json:"id"`
							Title string `json:"title"`
						} `json:"button_reply"`
					} `json:"interactive"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ReceiveWebhook parses a verified WhatsApp Cloud API event.
//
// Text messages prefer channel-role binding + Conversation linking
// (CreateRunInConversation) so plan review can reply in-thread. When no
// channel binding exists, falls back to TriggerEvent → onFire (legacy
// trigger-only setups). Interactive button replies resolve plan
// Approve/Deny without creating a new run.
func (p *Plugin) ReceiveWebhook(ctx context.Context, connectionID string, body []byte, _ map[string]string, onFire plugins.TriggerCallback) error {
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("parse whatsapp webhook: %w", err)
	}
	if payload.Object != "whatsapp_business_account" {
		// Status callbacks and unrelated objects are accepted but ignored.
		return nil
	}

	now := time.Now().UTC()
	fired := 0
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "messages" {
				continue
			}
			for _, msg := range change.Value.Messages {
				profileName := "WhatsApp user"
				for _, c := range change.Value.Contacts {
					if c.WAID == msg.From && c.Profile.Name != "" {
						profileName = c.Profile.Name
						break
					}
				}

				if msg.Type == "interactive" && msg.Interactive != nil &&
					msg.Interactive.Type == "button_reply" && msg.Interactive.ButtonReply != nil {
					p.handlePlanButtonReply(ctx, connectionID, msg.From, msg.Interactive.ButtonReply.ID)
					fired++
					continue
				}

				if msg.Type != "text" || msg.Text.Body == "" {
					// Media and other types are silently dropped so the
					// webhook still 200s and the platform doesn't retry.
					continue
				}

				handled, err := p.handleInboundText(ctx, connectionID, msg.From, profileName, msg.Text.Body)
				if err != nil {
					slog.Error("whatsapp: inbound text failed",
						"connection_id", connectionID, "message_id", msg.ID, "error", err)
					p.recordError(connectionID, err.Error())
					return err
				}
				if handled {
					fired++
					continue
				}

				// Legacy trigger path — no channel binding configured.
				event := plugins.TriggerEvent{
					ConnectionID: connectionID,
					Kind:         "whatsapp",
					Goal:         fmt.Sprintf("WhatsApp message from %s (%s): %s", profileName, msg.From, msg.Text.Body),
					Metadata: map[string]interface{}{
						"from":            msg.From,
						"profile_name":    profileName,
						"message_id":      msg.ID,
						"phone_number_id": change.Value.Metadata.PhoneNumberID,
						"display_phone":   change.Value.Metadata.DisplayPhoneNumber,
						"text":            msg.Text.Body,
					},
				}
				if err := onFire(ctx, event); err != nil {
					slog.Error("whatsapp: trigger fire failed",
						"connection_id", connectionID, "message_id", msg.ID, "error", err)
					p.recordError(connectionID, err.Error())
					return err
				}
				fired++
			}
		}
	}

	if fired > 0 {
		p.recordActivity(connectionID, now)
	}
	return nil
}

// handleInboundText creates a conversation-linked run when a channel-
// role binding exists. Returns handled=false so the caller can fall
// back to TriggerEvent when no channel assistant is bound.
func (p *Plugin) handleInboundText(ctx context.Context, connID, from, profileName, text string) (bool, error) {
	if p.rt == nil || p.bindings == nil {
		return false, nil
	}
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		return false, nil
	}
	if !p.senderAllowed(connID, assistantID, from) {
		p.handleFirstContact(connID, from, profileName)
		return true, nil // consumed (dropped / queued) — don't also fire trigger
	}

	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, from, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	goal := text
	if profileName != "" && profileName != "WhatsApp user" {
		goal = fmt.Sprintf("WhatsApp message from %s (%s): %s", profileName, from, text)
	}
	_, err = p.rt.CreateRunInConversation(ctx, goal, assistantID, "whatsapp", conversationID)
	if err != nil {
		return true, fmt.Errorf("create run: %w", err)
	}
	return true, nil
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

func (p *Plugin) handleFirstContact(connID, userID, display string) {
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
	case domain.FirstContactQueueApproval:
		if p.identities != nil && userID != "" {
			_ = p.identities.Create(&domain.ChannelIdentity{
				PluginID:           PluginID,
				ConnectionID:       connID,
				ExternalIdentifier: userID,
				DisplayName:        display,
				Enabled:            false,
			})
		}
	case domain.FirstContactDrop, domain.FirstContactReplyRequestAccess:
		log.Printf("[whatsapp plugin] dropped unknown sender on %s", connID)
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
