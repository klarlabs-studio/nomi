// Package discord implements the Discord channel plugin using the
// Gateway WebSocket (ADR 0001 + roady task discord-01/02). One
// Connection = one Discord bot application; users can create multiple
// connections if they run multiple bots.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.discord"

// Plugin implements plugins.Plugin + ChannelProvider + ConnectionHealthReporter
// for Discord using the Gateway WebSocket via discordgo.
type Plugin struct {
	rt            *runtime.Runtime
	connections   *db.ConnectionRepository
	bindings      *db.AssistantBindingRepository
	conversations *db.ConversationRepository
	identities    *db.ChannelIdentityRepository
	secrets       secrets.Store
	eventBus      *events.EventBus

	mu            sync.RWMutex
	running       bool
	sessions      map[string]*discordgo.Session // connection_id → session
	healthPerConn map[string]*plugins.ConnectionHealth
}

// NewPlugin wires the Discord plugin.
func NewPlugin(
	rt *runtime.Runtime,
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	convs *db.ConversationRepository,
	idents *db.ChannelIdentityRepository,
	secrets secrets.Store,
	eventBus *events.EventBus,
) *Plugin {
	return &Plugin{
		rt:            rt,
		connections:   conns,
		bindings:      binds,
		conversations: convs,
		identities:    idents,
		secrets:       secrets,
		eventBus:      eventBus,
	}
}

// Manifest declares the plugin contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Discord",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Discord bot integration via Gateway WebSocket. Users DM the bot or @mention it in guild channels.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"discord.post",
			"network.outgoing",
			"filesystem.read",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "discord",
				Description:       "Discord DM / @mention",
				SupportsThreading: true,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "discord.post_message",
				Capability:         "discord.post",
				Description:        "Post a message to a Discord channel. Inputs: connection_id, channel_id, text",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{{
				Kind:        "discord_bot_token",
				Key:         "bot_token",
				Label:       "Bot Token",
				Required:    true,
				Description: "Create a Discord application, add a bot, and paste the bot token here.",
			}},
			ConfigSchema: map[string]plugins.ConfigField{
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `"drop" | "reply_request_access" | "queue_approval".`,
				},
			},
			NetworkAllowlist: []string{"discord.com", "*.discord.com", "*.discordapp.net", "gateway.discord.gg"},
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

// Start opens a Gateway WebSocket per enabled connection.
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
		return fmt.Errorf("list discord connections: %w", err)
	}
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		p.startConnection(ctx, conn)
	}
	return nil
}

// Stop closes every session.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	for id, sess := range p.sessions {
		_ = sess.Close()
		log.Printf("[discord plugin] closed session for connection %s", id)
	}
	p.sessions = map[string]*discordgo.Session{}
	p.running = false
	return nil
}

// Channels returns one Channel per enabled connection.
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
		sess, ok := p.sessions[conn.ID]
		if !ok {
			continue
		}
		out = append(out, &Channel{connectionID: conn.ID, sess: sess})
	}
	return out
}

func (p *Plugin) startConnection(ctx context.Context, conn *domain.Connection) {
	token, err := p.resolveSecret(conn, "bot_token")
	if err != nil {
		log.Printf("[discord plugin] %v; skipping %s", err, conn.ID)
		return
	}
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Printf("[discord plugin] new session %s: %v", conn.ID, err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	// Intents: need MessageContent to read DMs + channel messages.
	sess.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

	connID := conn.ID
	sess.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		p.onMessage(ctx, connID, s, m)
	})
	if err := sess.Open(); err != nil {
		log.Printf("[discord plugin] open session %s: %v", conn.ID, err)
		p.recordConnectionError(conn.ID, err)
		return
	}
	p.mu.Lock()
	p.sessions[conn.ID] = sess
	p.mu.Unlock()
	p.recordConnectionSuccess(conn.ID)
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

// onMessage fires on every Gateway MESSAGE_CREATE event. Filters bot
// self-messages, enforces allowlist, and creates a Run.
func (p *Plugin) onMessage(ctx context.Context, connID string, s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore our own messages to avoid feedback loops.
	if s.State.User != nil && m.Author.ID == s.State.User.ID {
		return
	}
	// Ignore other bots by default.
	if m.Author.Bot {
		return
	}
	if m.Content == "" {
		return
	}

	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[discord plugin] %v; dropping message on %s", err, connID)
		return
	}
	if !p.senderAllowed(connID, assistantID, m.Author.ID) {
		p.handleFirstContact(ctx, connID, s, m.ChannelID, m.Author, p.firstContactPolicy(connID))
		return
	}

	// Thread key: channel_id captures DM/channel identity; we don't yet
	// model Discord threads as first-class (that's a polish follow-up).
	externalID := m.ChannelID

	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, externalID, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	run, err := p.rt.CreateRunInConversation(ctx, m.Content, assistantID, "discord", conversationID)
	if err != nil {
		log.Printf("[discord plugin] create run: %v", err)
		return
	}
	if atts := discordInboundAttachments(m); len(atts) > 0 {
		if err := p.rt.AttachToRun(run.ID, atts); err != nil {
			log.Printf("[discord plugin] attach to run %s: %v", run.ID, err)
		}
	}
}

// discordInboundAttachments converts the Discord MessageCreate's
// Attachments slice into RunAttachments. Discord gives us URLs directly
// (URL + ProxyURL); the enrichment pass uses URL since ProxyURL is for
// CDN caching.
func discordInboundAttachments(m *discordgo.MessageCreate) []*domain.RunAttachment {
	if m == nil || len(m.Attachments) == 0 {
		return nil
	}
	out := make([]*domain.RunAttachment, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		out = append(out, &domain.RunAttachment{
			Kind:        discordKindFromContentType(a.ContentType),
			Filename:    a.Filename,
			ContentType: a.ContentType,
			URL:         a.URL,
			ExternalID:  a.ID,
			SizeBytes:   int64(a.Size),
		})
	}
	return out
}

// discordKindFromContentType maps Discord's content_type field to our
// AttachmentKind enum. Discord populates content_type for most
// uploads; falls back to "document" when it doesn't.
func discordKindFromContentType(ct string) string {
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	}
	return "document"
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

func (p *Plugin) handleFirstContact(ctx context.Context, connID string, s *discordgo.Session, channelID string, author *discordgo.User, policy domain.FirstContactPolicy) {
	_ = ctx
	userID := ""
	display := ""
	if author != nil {
		userID = author.ID
		display = author.Username
	}
	switch policy {
	case domain.FirstContactReplyRequestAccess:
		if s != nil && channelID != "" {
			_, _ = s.ChannelMessageSend(channelID,
				"Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.")
		}
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
	case domain.FirstContactDrop:
		// silent drop
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

// --- health helpers ---

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

// Channel implements plugins.Channel for outbound Discord messages.
type Channel struct {
	connectionID string
	sess         *discordgo.Session
}

// ConnectionID returns the Connection this Channel is scoped to.
func (c *Channel) ConnectionID() string { return c.connectionID }

// Kind identifies the channel family.
func (c *Channel) Kind() string { return "discord" }

// Send posts a message to the Discord channel identified by
// externalConversationID. For DMs this is the DM channel id, for guild
// channels it's the text channel id.
//
// When the message carries attachments, falls through to
// ChannelMessageSendComplex which accepts a MessageSend with a Files
// slice — Discord renders files inline alongside the message text.
// Discord doesn't have an explicit "caption" concept on attachments,
// so per-attachment Captions are concatenated into the Content field
// after msg.Text.
func (c *Channel) Send(_ context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	if len(msg.Attachments) == 0 {
		_, err := c.sess.ChannelMessageSend(externalConversationID, msg.Text)
		return err
	}
	files := make([]*discordgo.File, 0, len(msg.Attachments))
	captions := make([]string, 0, len(msg.Attachments))
	for _, att := range msg.Attachments {
		if !att.IsInline() {
			return fmt.Errorf("discord channel: attachment without inline bytes is not supported (URL=%s)", att.URL)
		}
		filename := att.Filename
		if filename == "" {
			filename = discordFallbackFilename(att.Kind)
		}
		files = append(files, &discordgo.File{
			Name:        filename,
			ContentType: att.ContentType,
			Reader:      bytes.NewReader(att.Data),
		})
		if att.Caption != "" {
			captions = append(captions, att.Caption)
		}
	}
	content := msg.Text
	if len(captions) > 0 {
		if content != "" {
			content += "\n"
		}
		content += strings.Join(captions, "\n")
	}
	_, err := c.sess.ChannelMessageSendComplex(externalConversationID, &discordgo.MessageSend{
		Content: content,
		Files:   files,
	})
	return err
}

// discordFallbackFilename mirrors the Telegram/Slack helpers. Discord
// renders previews based on the filename extension so the right
// extension matters for inline rendering.
func discordFallbackFilename(kind plugins.AttachmentKind) string {
	switch kind {
	case plugins.AttachmentImage:
		return "image.png"
	case plugins.AttachmentAudio:
		return "voice.ogg"
	case plugins.AttachmentVideo:
		return "video.mp4"
	}
	return "file.bin"
}
