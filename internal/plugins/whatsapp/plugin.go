// Package whatsapp implements the WhatsApp Business Cloud API channel
// plugin (roady #123). One Connection = one phone number ID + access
// token pair issued through Meta's Business Manager. Inbound traffic
// arrives via webhooks (the webhooks/router.go path); outbound replies
// go through the Graph API client in send.go.
//
// Scope of v1: text messages in + out. Media (images, audio), interactive
// templates, and message status callbacks (delivered/read) are out of
// scope for v1 — additive without changing the wire contract.
package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	eventBus      *events.EventBus
	secrets       secrets.Store

	mu            sync.RWMutex
	running       bool
	healthPerConn map[string]*plugins.ConnectionHealth
}

// NewPlugin wires the WhatsApp plugin.
func NewPlugin(
	rt *runtime.Runtime,
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	convs *db.ConversationRepository,
	idents *db.ChannelIdentityRepository,
	eventBus *events.EventBus,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		rt:            rt,
		connections:   conns,
		bindings:      binds,
		conversations: convs,
		identities:    idents,
		eventBus:      eventBus,
		secrets:       secretStore,
		healthPerConn: map[string]*plugins.ConnectionHealth{},
	}
}

// Manifest declares the WhatsApp plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "WhatsApp",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "WhatsApp Business Cloud API integration. Inbound messages route to the bound assistant; the assistant can reply through the whatsapp.send_message tool.",
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

// Start primes per-connection health structs. WhatsApp uses inbound
// webhooks rather than a long-lived socket so there's nothing to spin up
// here beyond marking the plugin running.
func (p *Plugin) Start(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return nil
	}
	p.running = true
	conns, err := p.connections.ListByPlugin(PluginID)
	if err != nil {
		return fmt.Errorf("list whatsapp connections: %w", err)
	}
	for _, conn := range conns {
		if conn.Enabled {
			p.healthPerConn[conn.ID] = &plugins.ConnectionHealth{Running: true}
		}
	}
	return nil
}

// Stop marks the plugin not running. There are no goroutines or sockets
// to tear down — inbound stops automatically when the daemon's HTTP
// server shuts down.
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
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ReceiveWebhook parses a verified WhatsApp Cloud API event, fires one
// TriggerEvent per inbound text message, and updates the connection
// health on success.
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
				if msg.Type != "text" || msg.Text.Body == "" {
					// v1 handles text only; other message types are silently
					// dropped so the webhook still 200s and the platform
					// doesn't retry.
					continue
				}
				profileName := "WhatsApp user"
				for _, c := range change.Value.Contacts {
					if c.WAID == msg.From && c.Profile.Name != "" {
						profileName = c.Profile.Name
						break
					}
				}
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
