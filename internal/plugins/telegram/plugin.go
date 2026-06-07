// Package telegram implements the Telegram Bot API channel as a Plugin
// under the new architecture defined in ADR 0001. This is a
// behavior-preserving migration of internal/connectors/telegram.go —
// the poll loop, send semantics, and capability manifest are identical;
// the persistence shape moves to plugin_connections and routing moves
// through assistant_connection_bindings instead of the per-connection
// default_assistant_id field.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginID is the stable reverse-DNS identifier for this plugin. Used
// throughout the persistence layer and external APIs — do not change
// without a migration.
const PluginID = "com.nomi.telegram"

// Plugin is the Telegram Bot API plugin. It plays the ChannelProvider
// role: inbound messages create Runs via the runtime, outbound replies
// flow back through the same bot the message originated on.
type Plugin struct {
	// Dependencies injected from main():
	rt            *runtime.Runtime
	connections   *db.ConnectionRepository
	bindings      *db.AssistantBindingRepository
	conversations *db.ConversationRepository
	identities    *db.ChannelIdentityRepository
	runs          *db.RunRepository
	approvals     *permissions.Manager
	eventBus      *events.EventBus
	secrets       secrets.Store

	// Overridable for tests; production code always talks to Telegram's
	// real endpoint.
	apiBase string

	mu            sync.RWMutex
	running       bool
	cancelPerConn map[string]context.CancelFunc        // connection id -> cancel
	runConnMap    map[string]string                    // run id -> connection id
	healthPerConn map[string]*plugins.ConnectionHealth // connection id -> health
	// approvalMsg tracks the Telegram (chat_id, message_id) that hosted
	// the inline keyboard for a given approval_id so we can edit it in
	// place when the approval resolves.
	approvalMsg map[string]approvalMsgRef
	status      plugins.PluginStatus
}

// approvalMsgRef pins an approval-prompt message to the bot+chat that
// originated it so resolving can edit-in-place.
type approvalMsgRef struct {
	ConnectionID string
	ChatID       string
	MessageID    int
}

// NewPlugin constructs the plugin with the dependencies it needs at boot.
// The caller is expected to also register this plugin's manifest
// capabilities with the runtime's plugin-manifest-lookup after registration.
func NewPlugin(
	rt *runtime.Runtime,
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	convs *db.ConversationRepository,
	idents *db.ChannelIdentityRepository,
	runs *db.RunRepository,
	approvals *permissions.Manager,
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
		approvals:     approvals,
		eventBus:      eventBus,
		secrets:       secretStore,
		apiBase:       "https://api.telegram.org",
		cancelPerConn: map[string]context.CancelFunc{},
		runConnMap:    map[string]string{},
		healthPerConn: map[string]*plugins.ConnectionHealth{},
		approvalMsg:   map[string]approvalMsgRef{},
	}
}

// ConnectionHealth implements plugins.ConnectionHealthReporter. Returns
// the per-Connection health the poll loop maintains as it works.
func (p *Plugin) ConnectionHealth(connectionID string) (plugins.ConnectionHealth, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.healthPerConn[connectionID]
	if !ok || h == nil {
		return plugins.ConnectionHealth{}, false
	}
	// Return a copy so callers can't mutate through the map.
	return *h, true
}

// recordConnectionSuccess updates the per-Connection health on a
// successful poll tick: timestamps the last event, clears any running
// error, and ensures the Running flag is set.
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

// recordConnectionError stamps the most recent error onto the per-Connection
// health. ErrorCount grows until a subsequent success resets it.
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

// markConnectionStopped clears the Running flag when the poll loop exits.
func (p *Plugin) markConnectionStopped(connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.healthPerConn[connID]; ok {
		h.Running = false
	}
}

// Manifest describes the Telegram plugin to the host. Unchanged from the
// old connectors.ConnectorManifest — same capabilities, same role.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Telegram",
		Version:     "1.0.0",
		Author:      "Nomi",
		Description: "Telegram Bot API integration. Users message a bot, Nomi creates a run for the bound assistant, and replies flow back through the same bot.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"network.outgoing",
			"filesystem.read",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "telegram",
				Description:       "Telegram bot DM",
				SupportsThreading: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{{
				Kind:        "bot_token",
				Key:         "bot_token",
				Label:       "Bot Token",
				Required:    true,
				Description: "BotFather-issued bot API token.",
			}},
			ConfigSchema: map[string]plugins.ConfigField{
				"enabled": {
					Type:        "boolean",
					Label:       "Enabled",
					Required:    false,
					Default:     "false",
					Description: "Whether this plugin is active.",
				},
			},
			NetworkAllowlist: []string{"api.telegram.org", "*.t.me"},
		},
	}
}

// Configure is a no-op for the Telegram plugin — state lives in the
// ConnectionRepository which the plugin reads on each poll-loop tick, so
// hot reconfiguration happens without any plugin-level action. Present to
// satisfy the Plugin interface.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Status returns the runtime status for the Plugins tab.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	s := p.status
	s.Running = p.running
	s.Ready = true
	return s
}

// Start launches a polling goroutine for every enabled Telegram
// connection. Idempotent: calling Start on an already-running plugin is
// a no-op.
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
		return fmt.Errorf("list telegram connections: %w", err)
	}
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		p.startConnection(ctx, conn)
	}
	log.Printf("[telegram plugin] started %d active connection(s)", len(p.cancelPerConn))
	if p.eventBus != nil && p.approvals != nil && p.runs != nil {
		go p.subscribeApprovals(ctx)
	}
	return nil
}

// subscribeApprovals listens for approval.requested/resolved and drives
// inline-keyboard buttons in the originating Telegram chat. Mirrors the
// Slack plugin's approval UX so users get the same interactive approve/
// deny surface regardless of channel.
func (p *Plugin) subscribeApprovals(ctx context.Context) {
	sub := p.eventBus.Subscribe(events.EventFilter{
		EventTypes: []domain.EventType{
			domain.EventApprovalRequested,
			domain.EventApprovalResolved,
		},
	})
	defer sub.Unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sub.Events():
			if !ok {
				return
			}
			switch evt.Type {
			case domain.EventApprovalRequested:
				p.onApprovalRequested(ctx, evt)
			case domain.EventApprovalResolved:
				p.onApprovalResolved(ctx, evt)
			}
		}
	}
}

// onApprovalRequested posts an inline-keyboard message to the Telegram
// chat associated with the approval's run. Silently no-ops when the
// run isn't Telegram-originated.
func (p *Plugin) onApprovalRequested(ctx context.Context, evt *domain.Event) {
	approvalID, _ := evt.Payload["approval_id"].(string)
	capability, _ := evt.Payload["capability"].(string)
	if approvalID == "" {
		return
	}
	run, err := p.runs.GetByID(evt.RunID)
	if err != nil || run == nil || run.ConversationID == nil {
		return
	}
	conv, err := p.conversations.GetByID(*run.ConversationID)
	if err != nil || conv == nil || conv.PluginID != PluginID {
		return
	}
	chatID := conv.ExternalConversationID
	conn, err := p.connections.GetByID(conv.ConnectionID)
	if err != nil || conn == nil {
		return
	}
	token, err := p.resolveBotToken(conn)
	if err != nil {
		return
	}
	text := fmt.Sprintf("Nomi needs approval to use *%s*.", capability)
	msgID, err := p.sendApprovalPrompt(ctx, token, chatID, text, approvalID)
	if err != nil {
		log.Printf("[telegram plugin] post approval prompt: %v", err)
		return
	}
	p.mu.Lock()
	p.approvalMsg[approvalID] = approvalMsgRef{
		ConnectionID: conv.ConnectionID,
		ChatID:       chatID,
		MessageID:    msgID,
	}
	p.mu.Unlock()
}

// onApprovalResolved edits the original prompt to strip the buttons and
// show the resolution outcome. Keeps the chat clean: stale "Approve"
// buttons are worse UX than an in-place edit.
func (p *Plugin) onApprovalResolved(ctx context.Context, evt *domain.Event) {
	approvalID, _ := evt.Payload["approval_id"].(string)
	status, _ := evt.Payload["status"].(string)
	if approvalID == "" {
		return
	}
	p.mu.Lock()
	ref, ok := p.approvalMsg[approvalID]
	delete(p.approvalMsg, approvalID)
	p.mu.Unlock()
	if !ok {
		return
	}
	conn, err := p.connections.GetByID(ref.ConnectionID)
	if err != nil {
		return
	}
	token, err := p.resolveBotToken(conn)
	if err != nil {
		return
	}
	label := "Resolved"
	if status != "" {
		label = fmt.Sprintf("Resolved: %s", status)
	}
	_ = p.editMessageText(ctx, token, ref.ChatID, ref.MessageID, label)
}

// sendApprovalPrompt posts a message with a two-button inline keyboard.
// Returns the posted message_id so onApprovalResolved can edit it.
// callback_data encodes the approval action + id so handleCallback can
// route the click back to the ApprovalManager without state lookup.
func (p *Plugin) sendApprovalPrompt(ctx context.Context, token, chatID, text, approvalID string) (int, error) {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
		"reply_markup": map[string]interface{}{
			"inline_keyboard": [][]map[string]string{{
				{"text": "Approve", "callback_data": "nomi_approve:" + approvalID},
				{"text": "Deny", "callback_data": "nomi_deny:" + approvalID},
			}},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", p.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("telegram sendMessage returned %d", resp.StatusCode)
	}
	var decoded struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil || !decoded.OK {
		return 0, fmt.Errorf("decode sendMessage response")
	}
	return decoded.Result.MessageID, nil
}

// handleCallback routes an inline-keyboard tap. callback_data encodes
// the action + approval_id as "nomi_approve:<id>" or "nomi_deny:<id>"
// so we can parse without a lookup table. answerCallbackQuery is called
// unconditionally so the Telegram client's loading spinner clears.
func (p *Plugin) handleCallback(ctx context.Context, token, queryID, data string) {
	defer p.answerCallbackQuery(ctx, token, queryID)
	if p.approvals == nil {
		return
	}
	var approved bool
	var approvalID string
	switch {
	case strings.HasPrefix(data, "nomi_approve:"):
		approved = true
		approvalID = strings.TrimPrefix(data, "nomi_approve:")
	case strings.HasPrefix(data, "nomi_deny:"):
		approved = false
		approvalID = strings.TrimPrefix(data, "nomi_deny:")
	default:
		return
	}
	if approvalID == "" {
		return
	}
	if err := p.approvals.Resolve(ctx, approvalID, approved); err != nil {
		log.Printf("[telegram plugin] resolve approval %s: %v", approvalID, err)
	}
}

// answerCallbackQuery is a best-effort acknowledgement so the inline
// keyboard's spinner stops spinning on the user's device. Telegram
// requires one answerCallbackQuery per callback or the UI looks broken.
func (p *Plugin) answerCallbackQuery(ctx context.Context, token, queryID string) {
	payload := map[string]string{"callback_query_id": queryID}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/bot%s/answerCallbackQuery", p.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// editMessageText rewrites a previously-sent message's text, removing
// the inline keyboard by passing no reply_markup.
func (p *Plugin) editMessageText(ctx context.Context, token, chatID string, messageID int, text string) error {
	payload := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/editMessageText", p.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Stop cancels every running poll goroutine and marks the plugin stopped.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	for id, cancel := range p.cancelPerConn {
		cancel()
		log.Printf("[telegram plugin] stopped connection %s", id)
	}
	p.cancelPerConn = map[string]context.CancelFunc{}
	p.running = false
	return nil
}

// Channels returns one Channel per enabled connection. Called by the
// runtime when it needs to deliver an outbound message.
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
		out = append(out, &Channel{
			connectionID: conn.ID,
			token:        p.mustResolveToken(conn),
			apiBase:      p.apiBase,
		})
	}
	return out
}

func (p *Plugin) startConnection(ctx context.Context, conn *domain.Connection) {
	token, err := p.resolveBotToken(conn)
	if err != nil {
		log.Printf("[telegram plugin] %v; skipping connection %s", err, conn.ID)
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancelPerConn[conn.ID] = cancel
	p.mu.Unlock()
	go p.pollLoop(loopCtx, conn.ID, token)
}

// resolveBotToken dereferences the "bot_token" credential ref for a
// connection via the secrets store. Errors surface when the reference is
// malformed or the key has been revoked from the keyring.
func (p *Plugin) resolveBotToken(conn *domain.Connection) (string, error) {
	ref, ok := conn.CredentialRefs["bot_token"]
	if !ok || ref == "" {
		return "", fmt.Errorf("connection %q missing bot_token credential", conn.ID)
	}
	if p.secrets == nil {
		return ref, nil // tests with no secrets store fall through
	}
	plain, err := secrets.Resolve(p.secrets, ref)
	if err != nil {
		return "", fmt.Errorf("resolve bot_token for %s: %w", conn.ID, err)
	}
	if plain == "" {
		return "", fmt.Errorf("bot_token for %s is empty", conn.ID)
	}
	return plain, nil
}

// mustResolveToken is the infallible variant used inside Channel builds
// where we already know the connection is enabled + validated. Returns ""
// if resolution fails; the caller's Send() call will fail cleanly.
func (p *Plugin) mustResolveToken(conn *domain.Connection) string {
	tok, err := p.resolveBotToken(conn)
	if err != nil {
		return ""
	}
	return tok
}

// --- poll loop ---

type telegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text    string `json:"text"`
		Caption string `json:"caption"`
		From    struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		// Attachment payloads — Telegram delivers each kind under its
		// own field. Photo is an array of size variants; we keep all
		// and let downstream enrichment pick the largest.
		Photo []struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
		} `json:"photo,omitempty"`
		Voice *struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
			Duration int    `json:"duration"`
			MimeType string `json:"mime_type"`
		} `json:"voice,omitempty"`
		Audio *struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
			MimeType string `json:"mime_type"`
			FileName string `json:"file_name"`
		} `json:"audio,omitempty"`
		Video *struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
			MimeType string `json:"mime_type"`
		} `json:"video,omitempty"`
		Document *struct {
			FileID   string `json:"file_id"`
			FileSize int64  `json:"file_size"`
			MimeType string `json:"mime_type"`
			FileName string `json:"file_name"`
		} `json:"document,omitempty"`
	} `json:"message"`
	// CallbackQuery is the payload Telegram sends when a user taps an
	// inline-keyboard button. Used to resolve approvals.
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
	} `json:"callback_query"`
}

type telegramGetUpdatesResponse struct {
	OK     bool             `json:"ok"`
	Result []telegramUpdate `json:"result"`
}

func (p *Plugin) pollLoop(ctx context.Context, connID, token string) {
	client := &http.Client{Timeout: 65 * time.Second} // > Telegram's 60s long-poll timeout
	var offset int
	defer p.markConnectionStopped(connID)
	// Seed health as "running, no activity yet" so the UI doesn't flash
	// "never seen any events" briefly before the first poll returns.
	p.recordConnectionSuccess(connID)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&limit=10&timeout=60", p.apiBase, token, offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[telegram plugin] getUpdates error on %s: %v", connID, err)
			p.recordConnectionError(connID, err)
			time.Sleep(5 * time.Second)
			continue
		}
		var body telegramGetUpdatesResponse
		dec := json.NewDecoder(resp.Body)
		err = dec.Decode(&body)
		resp.Body.Close()
		if err != nil || !body.OK {
			if err != nil {
				log.Printf("[telegram plugin] decode updates on %s: %v", connID, err)
				p.recordConnectionError(connID, err)
			} else {
				p.recordConnectionError(connID, fmt.Errorf("telegram API returned not OK"))
			}
			time.Sleep(5 * time.Second)
			continue
		}
		// Successful round trip — refresh health.
		p.recordConnectionSuccess(connID)

		for _, update := range body.Result {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			if update.CallbackQuery != nil && update.CallbackQuery.Data != "" {
				// Route approval-button taps to the ApprovalManager.
				p.handleCallback(ctx, token, update.CallbackQuery.ID, update.CallbackQuery.Data)
				continue
			}
			if update.Message == nil {
				continue
			}
			// Build the text payload — Telegram delivers attachment
			// captions in update.Message.Caption rather than .Text.
			text := update.Message.Text
			if text == "" {
				text = update.Message.Caption
			}
			attachments := extractInboundAttachments(update.Message)
			// Skip updates that carry neither text nor attachments
			// (e.g. service messages like "user joined").
			if text == "" && len(attachments) == 0 {
				continue
			}
			chatID := fmt.Sprintf("%d", update.Message.Chat.ID)
			// Telegram identifies the sender separately from the chat — DMs
			// have sender_id == chat_id; groups don't. Pass the sender
			// identifier to the identity allowlist so per-user gating
			// still works in group chats.
			senderID := fmt.Sprintf("%d", update.Message.From.ID)
			displayName := update.Message.From.Username
			if displayName == "" {
				displayName = update.Message.From.FirstName
			}
			go p.handleMessage(ctx, connID, text, chatID, senderID, displayName, attachments)
		}
	}
}

// handleMessage resolves the assistant bound to this connection in the
// channel role, applies the identity allowlist, creates or continues a
// Conversation, and creates a Run threaded to that conversation.
// Captured inbound attachments are persisted via Runtime.AttachToRun
// so the enrichment pass (media-10) can transcribe / describe them
// before planning starts. Empty `message` is allowed when at least
// one attachment is present — attachment-only inbound (e.g. a voice
// note with no caption) is a normal Telegram interaction.
func (p *Plugin) handleMessage(ctx context.Context, connID, message, chatID, senderID, displayName string, attachments []*domain.RunAttachment) {
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[telegram plugin] %v; dropping message on %s", err, connID)
		_ = p.sendReply(connID, chatID, "This bot isn't linked to an assistant yet. Configure one in the Nomi desktop app under Plugins -> Telegram.")
		return
	}

	// Identity allowlist enforcement (ADR 0001 §9). If the plugin has
	// an identities repo AND the connection has any allowlist entries,
	// unknown senders get dropped per the connection's first-contact
	// policy. If no allowlist entries exist for this connection, treat
	// as "allow everyone" — this keeps Telegram backward-compatible
	// with existing bots that don't yet have an allowlist configured.
	if p.identities != nil && senderID != "" {
		existing, err := p.identities.ListByConnection(connID)
		if err == nil && len(existing) > 0 {
			allowed, _ := p.identities.IsAllowed(PluginID, connID, senderID, assistantID)
			if !allowed {
				log.Printf("[telegram plugin] blocking unknown sender %s on %s", senderID, connID)
				policy := p.firstContactPolicy(connID)
				p.handleFirstContact(ctx, connID, chatID, senderID, displayName, policy)
				return
			}
		}
	}

	// Find-or-create the Conversation for this (bot, chat) pair. Each
	// Telegram chat_id is its own thread — a user and a bot form one
	// persistent conversation regardless of how many runs flow through it.
	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, chatID, assistantID, p.eventBus)
		if err != nil {
			log.Printf("[telegram plugin] find/create conversation on %s (chat %s): %v", connID, chatID, err)
			// Fall through: run still creates, just without conversation linkage.
		} else {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	run, err := p.rt.CreateRunInConversation(ctx, message, assistantID, "telegram", conversationID)
	if err != nil {
		log.Printf("[telegram plugin] create run from %s: %v", connID, err)
		_ = p.sendReply(connID, chatID, "Couldn't start a run. Please try again.")
		return
	}
	if len(attachments) > 0 {
		if err := p.rt.AttachToRun(run.ID, attachments); err != nil {
			// Attachment persistence failure is non-fatal — the Run was
			// created, the assistant just won't have media to enrich.
			// Logged so operators can spot a regressing plugin path.
			log.Printf("[telegram plugin] attach to run %s: %v", run.ID, err)
		}
	}
	p.mu.Lock()
	p.runConnMap[run.ID] = connID
	p.mu.Unlock()
}

// extractInboundAttachments inspects a Telegram update.Message for
// attachment payloads and returns matching RunAttachment records. The
// URL field is left empty: Telegram returns file_id only, and converting
// to a URL costs an extra getFile API call. Enrichment fetches on
// demand using the external_id (file_id) when it actually needs bytes.
//
// For Photo arrays, Telegram delivers multiple size variants; we record
// the largest one (last in the array per the API spec) so enrichment
// works on the highest-quality version available.
func extractInboundAttachments(m *struct {
	MessageID int `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
	From    struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
	} `json:"from"`
	Photo []struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	} `json:"photo,omitempty"`
	Voice *struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		Duration int    `json:"duration"`
		MimeType string `json:"mime_type"`
	} `json:"voice,omitempty"`
	Audio *struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		MimeType string `json:"mime_type"`
		FileName string `json:"file_name"`
	} `json:"audio,omitempty"`
	Video *struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		MimeType string `json:"mime_type"`
	} `json:"video,omitempty"`
	Document *struct {
		FileID   string `json:"file_id"`
		FileSize int64  `json:"file_size"`
		MimeType string `json:"mime_type"`
		FileName string `json:"file_name"`
	} `json:"document,omitempty"`
}) []*domain.RunAttachment {
	if m == nil {
		return nil
	}
	var out []*domain.RunAttachment
	if len(m.Photo) > 0 {
		// Last variant is the highest-resolution.
		largest := m.Photo[len(m.Photo)-1]
		out = append(out, &domain.RunAttachment{
			Kind:        "image",
			ExternalID:  largest.FileID,
			SizeBytes:   largest.FileSize,
			ContentType: "image/jpeg",
		})
	}
	if m.Voice != nil {
		out = append(out, &domain.RunAttachment{
			Kind:        "audio",
			ExternalID:  m.Voice.FileID,
			SizeBytes:   m.Voice.FileSize,
			ContentType: m.Voice.MimeType,
			Filename:    "voice.ogg",
		})
	}
	if m.Audio != nil {
		out = append(out, &domain.RunAttachment{
			Kind:        "audio",
			ExternalID:  m.Audio.FileID,
			SizeBytes:   m.Audio.FileSize,
			ContentType: m.Audio.MimeType,
			Filename:    m.Audio.FileName,
		})
	}
	if m.Video != nil {
		out = append(out, &domain.RunAttachment{
			Kind:        "video",
			ExternalID:  m.Video.FileID,
			SizeBytes:   m.Video.FileSize,
			ContentType: m.Video.MimeType,
		})
	}
	if m.Document != nil {
		out = append(out, &domain.RunAttachment{
			Kind:        "document",
			ExternalID:  m.Document.FileID,
			SizeBytes:   m.Document.FileSize,
			ContentType: m.Document.MimeType,
			Filename:    m.Document.FileName,
		})
	}
	return out
}

// resolveChannelAssistant finds which assistant should receive inbound
// messages on a given connection. Prefers primary bindings; falls back to
// any enabled channel-role binding. Returns an error if none exist.
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

// sendReply is a convenience used when the plugin itself (not a run)
// needs to reply to the sender (e.g. when no assistant is bound).
func (p *Plugin) sendReply(connID, chatID, message string) error {
	ch := &Channel{connectionID: connID}
	ch.apiBase = p.apiBase
	conn, err := p.connections.GetByID(connID)
	if err != nil {
		return err
	}
	ch.token = p.mustResolveToken(conn)
	return ch.Send(context.Background(), chatID, plugins.OutboundMessage{Text: message})
}

// --- Channel ---

// Channel is the per-Connection send handle. One instance per enabled
// Telegram connection; returned from Plugin.Channels().
type Channel struct {
	connectionID string
	token        string
	apiBase      string
}

// ConnectionID returns the Connection this Channel is scoped to.
func (c *Channel) ConnectionID() string { return c.connectionID }

// Kind identifies the channel family.
func (c *Channel) Kind() string { return "telegram" }

// Send delivers a rich message (text + optional attachments) to a
// Telegram chat. externalConversationID is the Telegram chat_id.
//
// Routing rules:
//  1. No attachments → sendMessage with msg.Text.
//  2. One attachment → the kind-appropriate endpoint (sendPhoto /
//     sendDocument / sendVoice / sendVideo) with msg.Text folded into
//     the caption field. This matches Telegram's own UX: one message
//     equals one media + caption.
//  3. Multiple attachments → first goes with the caption, the rest are
//     posted as follow-on media without captions. Telegram has a
//     sendMediaGroup endpoint for true albums; we keep the simple
//     loop for v1 and revisit if users want album semantics.
func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	if c.token == "" {
		return fmt.Errorf("telegram channel %s has no token", c.connectionID)
	}
	if len(msg.Attachments) == 0 {
		if msg.Text == "" {
			return nil
		}
		return c.sendPlainText(ctx, externalConversationID, msg.Text)
	}
	for i, att := range msg.Attachments {
		caption := ""
		if i == 0 {
			// Preserve the effective caption semantics (msg.Text wins
			// for the first attachment; att.Caption acts as a fallback
			// when the caller didn't set msg.Text).
			if msg.Text != "" {
				caption = msg.Text
			} else {
				caption = att.Caption
			}
		} else {
			caption = att.Caption
		}
		if err := c.sendAttachment(ctx, externalConversationID, att, caption); err != nil {
			return err
		}
	}
	return nil
}

// sendPlainText dispatches via Telegram's sendMessage endpoint. Same
// wire shape as the pre-media-02 implementation.
func (c *Channel) sendPlainText(ctx context.Context, chatID, text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBase, c.token)
	payload := map[string]string{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "Markdown",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeTelegramError(resp)
	}
	return nil
}

// sendAttachment uploads one attachment via the kind-appropriate
// Telegram endpoint. Inline bytes go through multipart/form-data; a
// URL (for attachments echoed from inbound) goes through the
// application/json form with the URL as the file value.
func (c *Channel) sendAttachment(ctx context.Context, chatID string, att plugins.Attachment, caption string) error {
	endpoint, formField := telegramEndpointForAttachment(att.Kind)
	url := fmt.Sprintf("%s/bot%s/%s", c.apiBase, c.token, endpoint)

	if !att.IsInline() && att.URL != "" {
		// JSON path: Telegram accepts a URL as the file value.
		payload := map[string]string{
			"chat_id": chatID,
			formField: att.URL,
			"caption": caption,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("telegram %s: %w", endpoint, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return decodeTelegramError(resp)
		}
		return nil
	}

	// Multipart path: inline bytes uploaded as a file.
	if !att.IsInline() {
		return fmt.Errorf("telegram attachment has neither inline bytes nor URL")
	}
	body, contentType, err := buildMultipart(chatID, formField, caption, att)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeTelegramError(resp)
	}
	return nil
}

// telegramEndpointForAttachment maps the generic Kind to the right Bot
// API endpoint + form field name. Unrecognized kinds fall through to
// sendDocument — Telegram renders any file that way.
func telegramEndpointForAttachment(kind plugins.AttachmentKind) (endpoint, field string) {
	switch kind {
	case plugins.AttachmentImage:
		return "sendPhoto", "photo"
	case plugins.AttachmentAudio:
		return "sendVoice", "voice"
	case plugins.AttachmentVideo:
		return "sendVideo", "video"
	case plugins.AttachmentDocument:
		return "sendDocument", "document"
	}
	return "sendDocument", "document"
}

// buildMultipart constructs a multipart/form-data body for a Telegram
// file upload. Uses mime/multipart to get the boundary + encoding
// right — hand-rolling multipart is error-prone and the stdlib
// implementation is sufficient for our payload sizes.
func buildMultipart(chatID, formField, caption string, att plugins.Attachment) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", chatID); err != nil {
		return nil, "", err
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return nil, "", err
		}
	}
	filename := att.Filename
	if filename == "" {
		filename = fallbackFilename(att.Kind, att.ContentType)
	}
	part, err := writer.CreateFormFile(formField, filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(att.Data); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &body, writer.FormDataContentType(), nil
}

// fallbackFilename produces a reasonable filename when the caller
// didn't supply one. Extension derived from the attachment kind so
// Telegram's preview logic renders the file correctly.
func fallbackFilename(kind plugins.AttachmentKind, ct string) string {
	_ = ct
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

// decodeTelegramError reads an error response body and returns a Go
// error carrying the Bot API's `description` field when present. Keeps
// the status-code fallback so unparseable responses still produce a
// diagnosable error string.
func decodeTelegramError(resp *http.Response) error {
	var apiErr struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&apiErr)
	if apiErr.Description != "" {
		return fmt.Errorf("telegram API error: %s (status %d)", apiErr.Description, resp.StatusCode)
	}
	return fmt.Errorf("telegram API returned %d", resp.StatusCode)
}

// firstContactPolicy reads the per-connection first-contact policy
// from the connection's config blob. Defaults to Drop when absent or
// unrecognized — safest behavior when the operator hasn't configured
// it yet.
func (p *Plugin) firstContactPolicy(connID string) domain.FirstContactPolicy {
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

// handleFirstContact applies the connection's first-contact policy when
// an unknown sender reaches a bot. Reply and Approval modes ship basic
// behavior here; richer flows (approval dialog, rate-limited request
// emails) are polish tasks that build on this hook.
func (p *Plugin) handleFirstContact(ctx context.Context, connID, chatID, senderID, displayName string, policy domain.FirstContactPolicy) {
	_ = ctx
	switch policy {
	case domain.FirstContactReplyRequestAccess:
		_ = p.sendReply(connID, chatID,
			"Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.")
	case domain.FirstContactQueueApproval:
		// Seed a disabled identity row so the owner can review + enable
		// it from the Plugins tab. Keeps a paper trail of strangers
		// who tried to contact the bot.
		_ = p.identities.Create(&domain.ChannelIdentity{
			PluginID:           PluginID,
			ConnectionID:       connID,
			ExternalIdentifier: senderID,
			DisplayName:        displayName,
			Enabled:            false,
		})
	case domain.FirstContactDrop:
		// silent drop
	}
}

// RunConnection looks up which connection a given run belongs to. Used
// by the runtime's outbound delivery path: "the assistant wants to reply,
// route it to the channel the inbound came from."
func (p *Plugin) RunConnection(runID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.runConnMap[runID]
	return id, ok
}
