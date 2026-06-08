// Package email implements the Email plugin: a generic IMAP/SMTP
// conversational channel. Mirrors the Telegram plugin's shape
// (ChannelProvider + ConnectionHealthReporter) but polls a mailbox
// instead of long-polling the Telegram API.
//
// Scope for v1: channel role only (email in → Run, reply out via
// threaded SMTP). Tool and Trigger roles are follow-ups (tasks
// email-03 and email-04).
package email

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
	"go.klarlabs.de/nomi/internal/plugins/email/transport"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginID is the stable reverse-DNS identifier for this plugin.
const PluginID = "com.nomi.email"

// defaultPollInterval is the IMAP poll cadence when the connection's
// config doesn't override it. Matches the "generic mailbox" default most
// providers tolerate (Gmail/Outlook handle 60s fine; lower risks throttling).
const defaultPollInterval = 60 * time.Second

// Plugin is the Email plugin implementation.
type Plugin struct {
	rt            *runtime.Runtime
	connections   *db.ConnectionRepository
	bindings      *db.AssistantBindingRepository
	conversations *db.ConversationRepository
	identities    *db.ChannelIdentityRepository
	triggerRules  *db.EmailTriggerRepository
	secrets       secrets.Store
	eventBus      *events.EventBus

	mu            sync.RWMutex
	running       bool
	cancelPerConn map[string]context.CancelFunc
	uidWatermark  map[string]uint32 // connection_id -> highest UID processed
	healthPerConn map[string]*plugins.ConnectionHealth
}

// NewPlugin constructs the Email plugin with its required repository
// dependencies.
func NewPlugin(
	rt *runtime.Runtime,
	conns *db.ConnectionRepository,
	bindings *db.AssistantBindingRepository,
	convs *db.ConversationRepository,
	idents *db.ChannelIdentityRepository,
	triggerRepo *db.EmailTriggerRepository,
	secrets secrets.Store,
	eventBus *events.EventBus,
) *Plugin {
	return &Plugin{
		rt:            rt,
		connections:   conns,
		bindings:      bindings,
		conversations: convs,
		identities:    idents,
		triggerRules:  triggerRepo,
		secrets:       secrets,
		eventBus:      eventBus,
	}
}

// Manifest declares the Email plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Email",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Generic IMAP/SMTP email channel. Receive email at a configured inbox, thread replies via In-Reply-To/References.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"email.send",
			"network.outgoing",
			"filesystem.read",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "email",
				Description:       "Email inbox",
				SupportsThreading: true,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "email.send",
				Capability:         "email.send",
				Description:        "Send an email from a connected mailbox. Inputs: connection_id, to, subject, body, in_reply_to?, references?",
				RequiresConnection: true,
			}},
			Triggers: []plugins.TriggerContribution{{
				Kind:        "inbox_watch",
				EventType:   "email.rule_matched",
				Description: "Fire a run against a named assistant when an inbound message matches a user-defined filter (from/subject/body substring).",
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{{
				Kind:        "imap_password",
				Key:         "password",
				Label:       "Password or app-specific password",
				Required:    true,
				Description: "IMAP/SMTP auth password. Gmail/Outlook require an app password or OAuth.",
			}},
			ConfigSchema: map[string]plugins.ConfigField{
				"imap_host": {
					Type: "string", Label: "IMAP Host", Required: true,
					Description: "e.g. imap.gmail.com, outlook.office365.com, imap.fastmail.com",
				},
				"imap_port": {Type: "number", Label: "IMAP Port", Default: "993"},
				"smtp_host": {
					Type: "string", Label: "SMTP Host", Required: true,
					Description: "e.g. smtp.gmail.com, smtp.office365.com, smtp.fastmail.com",
				},
				"smtp_port": {Type: "number", Label: "SMTP Port", Default: "587"},
				"username": {
					Type: "string", Label: "Username (email address)", Required: true,
				},
				"poll_interval_seconds": {
					Type: "number", Label: "Poll interval (seconds)", Default: "60",
					Description: "How often to check for new mail. Lower values may be throttled.",
				},
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `How to handle senders not in the allowlist. One of: "drop", "reply_request_access", "queue_approval".`,
				},
			},
			// Empty by design: per-connection imap_host/smtp_host
			// supplement the runtime allowlist (a user with two
			// accounts on Gmail and Fastmail must not be locked into a
			// single hard-coded host pair). The runtime adds them at
			// connection-start time.
			NetworkAllowlist: nil,
		},
	}
}

// Configure is a no-op — state lives in the ConnectionRepository and is
// reread on every poll tick.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Status reports plugin-level status.
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

// Start launches a poll goroutine for every enabled email connection.
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
		return fmt.Errorf("list email connections: %w", err)
	}
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		p.startConnection(ctx, conn)
	}
	return nil
}

// Stop cancels every poll goroutine.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	for _, cancel := range p.cancelPerConn {
		cancel()
	}
	p.cancelPerConn = map[string]context.CancelFunc{}
	p.running = false
	return nil
}

// Channels returns one Channel per active connection for outbound sends.
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
		cfg, err := p.buildTransportConfig(conn)
		if err != nil {
			continue
		}
		out = append(out, &Channel{connectionID: conn.ID, cfg: cfg})
	}
	return out
}

func (p *Plugin) startConnection(ctx context.Context, conn *domain.Connection) {
	cfg, err := p.buildTransportConfig(conn)
	if err != nil {
		log.Printf("[email plugin] %v; skipping connection %s", err, conn.ID)
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancelPerConn[conn.ID] = cancel
	p.mu.Unlock()
	go p.pollLoop(loopCtx, conn.ID, cfg, p.pollIntervalFor(conn))
}

func (p *Plugin) pollIntervalFor(conn *domain.Connection) time.Duration {
	if v, ok := conn.Config["poll_interval_seconds"].(float64); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultPollInterval
}

func (p *Plugin) buildTransportConfig(conn *domain.Connection) (transport.Config, error) {
	host, _ := conn.Config["imap_host"].(string)
	if host == "" {
		return transport.Config{}, fmt.Errorf("connection %s missing imap_host", conn.ID)
	}
	smtpHost, _ := conn.Config["smtp_host"].(string)
	if smtpHost == "" {
		return transport.Config{}, fmt.Errorf("connection %s missing smtp_host", conn.ID)
	}
	username, _ := conn.Config["username"].(string)
	if username == "" {
		return transport.Config{}, fmt.Errorf("connection %s missing username", conn.ID)
	}
	imapPort := intFromConfig(conn.Config, "imap_port", 993)
	smtpPort := intFromConfig(conn.Config, "smtp_port", 587)
	// Resolve the password via secrets.Store.
	ref, ok := conn.CredentialRefs["password"]
	if !ok || ref == "" {
		return transport.Config{}, fmt.Errorf("connection %s missing password credential", conn.ID)
	}
	password := ref
	if p.secrets != nil {
		pw, err := secrets.Resolve(p.secrets, ref)
		if err != nil {
			return transport.Config{}, fmt.Errorf("resolve password: %w", err)
		}
		password = pw
	}
	return transport.Config{
		IMAPHost: host,
		IMAPPort: imapPort,
		SMTPHost: smtpHost,
		SMTPPort: smtpPort,
		Username: username,
		Password: password,
		From:     username,
	}, nil
}

func intFromConfig(m map[string]interface{}, key string, def int) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	if s, ok := m[key].(string); ok && s != "" {
		var n int
		_, _ = fmt.Sscanf(s, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return def
}

func (p *Plugin) pollLoop(ctx context.Context, connID string, cfg transport.Config, interval time.Duration) {
	defer p.markConnectionStopped(connID)
	// Seed with immediate health + first tick.
	p.recordConnectionSuccess(connID)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run one cycle immediately so the user sees activity fast.
	p.pollOnce(ctx, connID, cfg)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollOnce(ctx, connID, cfg)
		}
	}
}

func (p *Plugin) pollOnce(ctx context.Context, connID string, cfg transport.Config) {
	p.mu.RLock()
	since := p.uidWatermark[connID]
	p.mu.RUnlock()

	msgs, nextUID, err := transport.FetchNew(ctx, cfg, since)
	if err != nil {
		log.Printf("[email plugin] fetch error on %s: %v", connID, err)
		p.recordConnectionError(connID, err)
		return
	}
	p.recordConnectionSuccess(connID)
	if nextUID > since {
		p.mu.Lock()
		p.uidWatermark[connID] = nextUID
		p.mu.Unlock()
	}
	for _, m := range msgs {
		p.handleMessage(ctx, connID, cfg, m)
	}
}

// handleMessage implements the inbound-to-Run flow: check trigger rules
// first, then fall through to the channel-role binding. Identity
// allowlist applies in both paths. Conversation threading is keyed by
// Message-ID root so users see one coherent thread regardless of which
// path produced the Run.
func (p *Plugin) handleMessage(ctx context.Context, connID string, cfg transport.Config, m transport.Message) {
	// Trigger rules short-circuit the channel-role flow. A matched rule
	// fires a Run against the rule's target assistant and returns.
	conn, _ := p.connections.GetByID(connID)
	if conn != nil && p.triggerRules != nil {
		rules, err := p.triggerRules.ListByConnection(connID)
		if err != nil {
			log.Printf("[email plugin] failed to list trigger rules: %v", err)
		} else if rule := firstMatchingRule(rules, m); rule != nil {
			// Identity allowlist still applies — the rule's assistant is
			// the target, but unknown senders are still gated.
			senderAddr, _ := transport.ParseAddress(m.From)
			if senderAddr == "" {
				senderAddr = m.From
			}
			if p.identities != nil && senderAddr != "" {
				existing, err := p.identities.ListByConnection(connID)
				if err == nil && len(existing) > 0 {
					allowed, _ := p.identities.IsAllowed(PluginID, connID, senderAddr, rule.AssistantID)
					if !allowed {
						log.Printf("[email plugin] rule %q matched but sender %s not allowlisted", rule.Name, senderAddr)
						return
					}
				}
			}
			threadKey := resolveThreadKey(m)
			var conv *domain.Conversation
			if p.conversations != nil && threadKey != "" {
				c, _, err := p.conversations.FindOrCreate(PluginID, connID, threadKey, rule.AssistantID, p.eventBus)
				if err == nil {
					conv = c
					_ = p.conversations.Touch(c.ID, p.eventBus)
				}
			}
			p.handleRuleMatch(ctx, rule, conv, m)
			return
		}
	}

	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[email plugin] %v; dropping message on %s", err, connID)
		return
	}

	senderAddr, _ := transport.ParseAddress(m.From)
	if senderAddr == "" {
		senderAddr = m.From
	}

	// Identity allowlist enforcement.
	if p.identities != nil && senderAddr != "" {
		existing, err := p.identities.ListByConnection(connID)
		if err == nil && len(existing) > 0 {
			allowed, _ := p.identities.IsAllowed(PluginID, connID, senderAddr, assistantID)
			if !allowed {
				log.Printf("[email plugin] blocking unknown sender %s on %s", senderAddr, connID)
				p.handleFirstContact(ctx, cfg, connID, senderAddr, m, p.firstContactPolicy(connID))
				return
			}
		}
	}

	threadKey := resolveThreadKey(m)

	var conversationID string
	if p.conversations != nil && threadKey != "" {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, threadKey, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	goal := m.Subject
	if goal == "" {
		goal = "(no subject)"
	}
	if m.Body != "" {
		goal = fmt.Sprintf("%s\n\nFrom: %s\n\n%s", goal, senderAddr, strings.TrimSpace(m.Body))
	}

	_, err = p.rt.CreateRunInConversation(ctx, goal, assistantID, "email", conversationID)
	if err != nil {
		log.Printf("[email plugin] create run from %s: %v", connID, err)
	}
}

// resolveChannelAssistant finds the assistant bound to this connection in the
// channel role. Prefers primary bindings, falls back to any enabled one.
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

func (p *Plugin) handleFirstContact(_ context.Context, cfg transport.Config, connID, senderAddr string, m transport.Message, policy domain.FirstContactPolicy) {
	switch policy {
	case domain.FirstContactReplyRequestAccess:
		if senderAddr != "" {
			_ = transport.SendEmail(cfg, []string{senderAddr},
				"Re: "+m.Subject,
				"Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.",
				m.MessageID, m.References)
		}
	case domain.FirstContactQueueApproval:
		if p.identities != nil {
			_ = p.identities.Create(&domain.ChannelIdentity{
				PluginID:           PluginID,
				ConnectionID:       connID,
				ExternalIdentifier: senderAddr,
				DisplayName:        m.From,
				Enabled:            false,
			})
		}
	case domain.FirstContactDrop:
		// silent drop
	}
}

// --- health helpers (mirrors Telegram plugin) ---

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

func (p *Plugin) markConnectionStopped(connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.healthPerConn[connID]; ok {
		h.Running = false
	}
}

// Channel is the per-connection outbound handle returned from Channels().
type Channel struct {
	connectionID string
	cfg          transport.Config
}

// ConnectionID returns the Connection this Channel is scoped to.
func (c *Channel) ConnectionID() string { return c.connectionID }

// Kind identifies the channel family.
func (c *Channel) Kind() string { return "email" }

// Send delivers a reply email, optionally with attachments.
// externalConversationID is the Message-ID of the conversation root;
// the reply uses In-Reply-To + References to thread properly.
// Recipient is currently parsed from the message body prefix; when
// attachments are present we route through SendEmailWithAttachments
// which builds a multipart/mixed body with base64-encoded parts.
func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	_ = ctx
	to, body := extractRecipientAndBody(msg.Text)
	if to == "" {
		return fmt.Errorf("email.Send: recipient could not be resolved from message; prefix with 'To: addr@host'")
	}
	if len(msg.Attachments) == 0 {
		return transport.SendEmail(c.cfg, []string{to}, "Re: (thread)", body, externalConversationID, []string{externalConversationID})
	}
	atts := make([]transport.EmailAttachment, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		if !a.IsInline() {
			return fmt.Errorf("email channel: attachment without inline bytes is not supported (URL=%s)", a.URL)
		}
		atts = append(atts, transport.EmailAttachment{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Data:        a.Data,
		})
	}
	return transport.SendEmailWithAttachments(c.cfg, []string{to}, "Re: (thread)", body, externalConversationID, []string{externalConversationID}, atts)
}

// extractRecipientAndBody parses a leading `To: <addr>\n` prefix from the
// message body, returning the recipient + remaining body. When the prefix
// is absent, returns ("", message) so the caller can surface a clear
// error rather than silently dropping the send.
func extractRecipientAndBody(message string) (string, string) {
	if !strings.HasPrefix(message, "To: ") && !strings.HasPrefix(message, "to: ") {
		return "", message
	}
	nl := strings.Index(message, "\n")
	if nl < 0 {
		return "", message
	}
	addr := strings.TrimSpace(message[len("To: "):nl])
	body := strings.TrimLeft(message[nl+1:], "\r\n")
	return addr, body
}
