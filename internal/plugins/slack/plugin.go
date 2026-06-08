// Package slack implements the Slack channel plugin using Socket Mode
// (ADR 0001 + roady task slack-01/02). One Connection = one installed
// Slack app; multiple workspaces coexist cleanly because each app
// installation has its own bot+app-level token pair.
//
// Socket Mode is chosen over the HTTP Events API because it avoids
// requiring the user to host an inbound webhook endpoint — a hard
// requirement for non-techies on the product's target persona.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/runtime"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// PluginID is the stable reverse-DNS identifier.
const PluginID = "com.nomi.slack"

// Plugin implements plugins.Plugin + ChannelProvider + ConnectionHealthReporter
// for Slack using Socket Mode.
type Plugin struct {
	rt            *runtime.Runtime
	connections   *db.ConnectionRepository
	bindings      *db.AssistantBindingRepository
	conversations *db.ConversationRepository
	identities    *db.ChannelIdentityRepository
	runs          *db.RunRepository
	approvals     *permissions.Manager
	eventBus      *events.EventBus
	secrets       secrets.Store

	mu            sync.RWMutex
	running       bool
	cancelPerConn map[string]context.CancelFunc
	clients       map[string]*slack.Client // cached API client per connection
	healthPerConn map[string]*plugins.ConnectionHealth
	// approvalMsgTS tracks the Slack message (channel, ts) that hosted
	// the approve/deny buttons for a given approval_id, so we can
	// update it in place once the approval resolves.
	approvalMsgTS map[string]approvalMsgRef
}

type approvalMsgRef struct {
	ConnectionID string
	Channel      string
	TS           string
}

// NewPlugin wires the Slack plugin.
//
// runs, approvals, and eventBus are optional at construction (pass nil
// to skip interactive-approval integration). When all three are supplied,
// the plugin subscribes to approval.requested events and renders Block
// Kit Approve/Deny buttons in the originating Slack thread; button
// clicks resolve the approval via the ApprovalManager.
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
		cancelPerConn: map[string]context.CancelFunc{},
		clients:       map[string]*slack.Client{},
		healthPerConn: map[string]*plugins.ConnectionHealth{},
		approvalMsgTS: map[string]approvalMsgRef{},
	}
}

// Manifest declares the Slack plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Slack",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Slack workspace integration via Socket Mode. Users DM the bot or @mention it in channels; replies thread through the same workspace.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"slack.post",
			"network.outgoing",
			"filesystem.read",
		},
		Contributes: plugins.Contributions{
			Channels: []plugins.ChannelContribution{{
				Kind:              "slack",
				Description:       "Slack DM / channel mention",
				SupportsThreading: true,
			}},
			Tools: []plugins.ToolContribution{{
				Name:               "slack.post_message",
				Capability:         "slack.post",
				Description:        "Post a message to a Slack channel or user DM. Inputs: connection_id, channel, text, thread_ts?",
				RequiresConnection: true,
			}},
		},
		Requires: plugins.Requirements{
			Credentials: []plugins.CredentialSpec{
				{
					Kind:        "slack_bot_token",
					Key:         "bot_token",
					Label:       "Bot Token (xoxb-…)",
					Required:    true,
					Description: "Install a Slack app to your workspace and paste the Bot Token from OAuth & Permissions.",
				},
				{
					Kind:        "slack_app_token",
					Key:         "app_token",
					Label:       "App-Level Token (xapp-…)",
					Required:    true,
					Description: "App-Level Token with the connections:write scope for Socket Mode.",
				},
			},
			ConfigSchema: map[string]plugins.ConfigField{
				"first_contact_policy": {
					Type: "string", Label: "Unknown-sender policy",
					Default:     "drop",
					Description: `How to handle Slack users not in the allowlist: "drop", "reply_request_access", "queue_approval".`,
				},
			},
			NetworkAllowlist: []string{"slack.com", "*.slack.com", "*.slack-edge.com"},
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

// Start opens a Socket Mode session for each enabled connection and,
// when the optional approvals+eventBus deps are set, subscribes to
// approval.requested events to drive Block Kit interactive approvals.
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
		return fmt.Errorf("list slack connections: %w", err)
	}
	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		p.startConnection(ctx, conn)
	}
	if p.eventBus != nil && p.approvals != nil && p.runs != nil {
		go p.subscribeApprovals(ctx)
	}
	return nil
}

// subscribeApprovals listens for approval-lifecycle events and posts
// Block Kit buttons into the Slack thread that originated the run.
// Resolved events update the originally-posted message to strip the
// buttons so the thread reads cleanly in retrospect.
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

// onApprovalRequested posts Block Kit Approve/Deny buttons to the Slack
// thread associated with the approval's run. Silently no-ops when the
// run isn't Slack-originated.
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

	channel, threadTS, err := splitExternalID(conv.ExternalConversationID)
	if err != nil {
		return
	}

	p.mu.RLock()
	client, ok := p.clients[conv.ConnectionID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	text := fmt.Sprintf("Nomi needs approval to use *%s*.", capability)
	blocks := buildApprovalBlocks(text, approvalID)
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	postedChannel, postedTS, err := client.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		log.Printf("[slack plugin] post approval block: %v", err)
		return
	}
	p.mu.Lock()
	p.approvalMsgTS[approvalID] = approvalMsgRef{
		ConnectionID: conv.ConnectionID,
		Channel:      postedChannel,
		TS:           postedTS,
	}
	p.mu.Unlock()
}

// onApprovalResolved rewrites the originally-posted approval message to
// strip the buttons and note the resolution outcome. Keeps the Slack
// thread tidy — stale "Approve" buttons that no longer do anything are
// worse UX than the in-place edit.
func (p *Plugin) onApprovalResolved(ctx context.Context, evt *domain.Event) {
	approvalID, _ := evt.Payload["approval_id"].(string)
	status, _ := evt.Payload["status"].(string)
	if approvalID == "" {
		return
	}
	p.mu.Lock()
	ref, ok := p.approvalMsgTS[approvalID]
	delete(p.approvalMsgTS, approvalID)
	p.mu.Unlock()
	if !ok {
		return
	}
	p.mu.RLock()
	client, ok := p.clients[ref.ConnectionID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	label := "Resolved"
	if status != "" {
		label = fmt.Sprintf("Resolved: %s", status)
	}
	_, _, _, err := client.UpdateMessageContext(ctx, ref.Channel, ref.TS, slack.MsgOptionText(label, false))
	if err != nil {
		log.Printf("[slack plugin] update resolved approval message: %v", err)
	}
}

// buildApprovalBlocks builds the interactive Block Kit message layout.
// Uses two buttons with action ids encoding (action, approval_id) so
// handleInteractive can parse them without a separate lookup table.
func buildApprovalBlocks(text, approvalID string) []slack.Block {
	approveBtn := slack.NewButtonBlockElement(
		"nomi_approve:"+approvalID,
		approvalID,
		slack.NewTextBlockObject(slack.PlainTextType, "Approve", false, false),
	)
	approveBtn.Style = slack.StylePrimary
	denyBtn := slack.NewButtonBlockElement(
		"nomi_deny:"+approvalID,
		approvalID,
		slack.NewTextBlockObject(slack.PlainTextType, "Deny", false, false),
	)
	denyBtn.Style = slack.StyleDanger

	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil, nil,
		),
		slack.NewActionBlock("nomi_approval_actions", approveBtn, denyBtn),
	}
}

// Stop tears down every connection's Socket Mode session.
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

// Channels returns one Channel per enabled connection for outbound sends.
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
		client, err := p.resolveClient(conn)
		if err != nil {
			continue
		}
		out = append(out, &Channel{connectionID: conn.ID, client: client})
	}
	return out
}

func (p *Plugin) startConnection(ctx context.Context, conn *domain.Connection) {
	botToken, err := p.resolveSecret(conn, "bot_token")
	if err != nil {
		log.Printf("[slack plugin] %v; skipping connection %s", err, conn.ID)
		return
	}
	appToken, err := p.resolveSecret(conn, "app_token")
	if err != nil {
		log.Printf("[slack plugin] %v; skipping connection %s", err, conn.ID)
		return
	}

	client := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	p.mu.Lock()
	p.clients[conn.ID] = client
	p.mu.Unlock()

	sm := socketmode.New(client)
	loopCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancelPerConn[conn.ID] = cancel
	p.mu.Unlock()

	go p.runSocketMode(loopCtx, conn.ID, client, sm)
}

func (p *Plugin) resolveClient(conn *domain.Connection) (*slack.Client, error) {
	p.mu.RLock()
	c, ok := p.clients[conn.ID]
	p.mu.RUnlock()
	if ok {
		return c, nil
	}
	botToken, err := p.resolveSecret(conn, "bot_token")
	if err != nil {
		return nil, err
	}
	appToken, _ := p.resolveSecret(conn, "app_token")
	opts := []slack.Option{}
	if appToken != "" {
		opts = append(opts, slack.OptionAppLevelToken(appToken))
	}
	client := slack.New(botToken, opts...)
	p.mu.Lock()
	p.clients[conn.ID] = client
	p.mu.Unlock()
	return client, nil
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

// runSocketMode is the long-running goroutine that reads Socket Mode
// events and dispatches them. Exits when the context is cancelled.
func (p *Plugin) runSocketMode(ctx context.Context, connID string, client *slack.Client, sm *socketmode.Client) {
	defer p.markConnectionStopped(connID)
	p.recordConnectionSuccess(connID)

	errCh := make(chan error, 1)
	go func() {
		errCh <- sm.RunContext(ctx)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				log.Printf("[slack plugin] socket mode exited on %s: %v", connID, err)
				p.recordConnectionError(connID, err)
			}
			return
		case evt, ok := <-sm.Events:
			if !ok {
				return
			}
			p.handleSocketEvent(ctx, connID, client, sm, evt)
		}
	}
}

// handleSocketEvent dispatches based on event type. We handle a small
// curated set for v1: EventsAPI message events (DMs + mentions). Slash
// commands and interactive components land in slack-02/03.
func (p *Plugin) handleSocketEvent(ctx context.Context, connID string, client *slack.Client, sm *socketmode.Client, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting, socketmode.EventTypeConnected:
		p.recordConnectionSuccess(connID)
	case socketmode.EventTypeEventsAPI:
		apiEvt, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		// Always ack so Slack doesn't retry.
		_ = sm.Ack(*evt.Request)
		p.handleEventsAPI(ctx, connID, client, apiEvt)
	case socketmode.EventTypeInteractive:
		callback, ok := evt.Data.(slack.InteractionCallback)
		if !ok {
			return
		}
		// Ack immediately so Slack shows the button press as handled.
		_ = sm.Ack(*evt.Request)
		p.handleInteraction(ctx, connID, callback)
	case socketmode.EventTypeErrorWriteFailed,
		socketmode.EventTypeErrorBadMessage,
		socketmode.EventTypeDisconnect:
		p.recordConnectionError(connID, fmt.Errorf("slack socket event: %s", evt.Type))
	}
}

func (p *Plugin) handleEventsAPI(ctx context.Context, connID string, client *slack.Client, apiEvt slackevents.EventsAPIEvent) {
	if apiEvt.Type != slackevents.CallbackEvent {
		return
	}
	switch e := apiEvt.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		// Ignore bot's own messages and edits to avoid loops.
		if e.BotID != "" || e.SubType != "" {
			return
		}
		if e.Text == "" {
			return
		}
		p.handleMessage(ctx, connID, client, e)
	case *slackevents.AppMentionEvent:
		p.handleMention(ctx, connID, client, e)
	}
}

// handleMessage resolves the assistant, applies the allowlist, threads
// into a Conversation keyed on the Slack channel+thread, and creates a
// Run. Captured Files are persisted as RunAttachments. Outbound
// replies flow back via Channel.Send.
func (p *Plugin) handleMessage(ctx context.Context, connID string, client *slack.Client, e *slackevents.MessageEvent) {
	assistantID, err := p.resolveChannelAssistant(connID)
	if err != nil {
		log.Printf("[slack plugin] %v; dropping message on %s", err, connID)
		return
	}
	if !p.senderAllowed(connID, assistantID, e.User) {
		log.Printf("[slack plugin] blocking unknown sender %s on %s", e.User, connID)
		p.handleFirstContact(ctx, connID, client, e.Channel, e.User, p.firstContactPolicy(connID))
		return
	}

	// Thread key: prefer thread_ts (existing thread) else ts (new thread
	// rooted at this message).
	threadKey := e.ThreadTimeStamp
	if threadKey == "" {
		threadKey = e.TimeStamp
	}
	// Include the channel in the external conversation id so DMs and
	// channel threads stay separate even if they reuse timestamps.
	externalID := fmt.Sprintf("%s:%s", e.Channel, threadKey)

	var conversationID string
	if p.conversations != nil {
		conv, _, err := p.conversations.FindOrCreate(PluginID, connID, externalID, assistantID, p.eventBus)
		if err == nil {
			conversationID = conv.ID
			_ = p.conversations.Touch(conv.ID, p.eventBus)
		}
	}

	run, err := p.rt.CreateRunInConversation(ctx, e.Text, assistantID, "slack", conversationID)
	if err != nil {
		log.Printf("[slack plugin] create run from %s: %v", connID, err)
		return
	}
	if e.Message != nil && len(e.Message.Files) > 0 {
		if attachments := slackInboundAttachments(e.Message.Files); len(attachments) > 0 {
			if err := p.rt.AttachToRun(run.ID, attachments); err != nil {
				log.Printf("[slack plugin] attach to run %s: %v", run.ID, err)
			}
		}
	}
}

// slackInboundAttachments turns slack.File entries into RunAttachments.
// Slack gives us URLs directly (URLPrivate / URLPrivateDownload) so the
// enrichment pass can fetch without an extra round trip. Note that
// Slack's MessageEvent carries Files inside the embedded Msg struct
// (only populated on certain subtypes — file_share is the main one).
// File-only messages with no embedded Msg surface a separate file_share
// event that is out of scope for v1; we'll wire it in a follow-up.
func slackInboundAttachments(files []slack.File) []*domain.RunAttachment {
	out := make([]*domain.RunAttachment, 0, len(files))
	for _, f := range files {
		out = append(out, &domain.RunAttachment{
			Kind:        slackKindFromMime(f.Mimetype),
			Filename:    f.Name,
			ContentType: f.Mimetype,
			URL:         f.URLPrivate,
			ExternalID:  f.ID,
			SizeBytes:   int64(f.Size),
		})
	}
	return out
}

// slackKindFromMime maps an RFC 6838 mime type to our coarse
// AttachmentKind enum. Unknown types fall back to "document" so the
// enrichment pass at least knows it's a binary file.
func slackKindFromMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	}
	return "document"
}

func (p *Plugin) handleMention(ctx context.Context, connID string, client *slack.Client, e *slackevents.AppMentionEvent) {
	// Mentions are like messages but carry a guaranteed user + channel;
	// reuse handleMessage after adapting fields.
	synth := &slackevents.MessageEvent{
		User:            e.User,
		Channel:         e.Channel,
		Text:            e.Text,
		TimeStamp:       e.TimeStamp,
		ThreadTimeStamp: e.ThreadTimeStamp,
	}
	p.handleMessage(ctx, connID, client, synth)
}

// senderAllowed wraps the identity-allowlist check. Empty allowlist → allow
// (backward-compat with the Telegram plugin's semantics).
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

func (p *Plugin) handleFirstContact(ctx context.Context, connID string, client *slack.Client, channelID, userID string, policy domain.FirstContactPolicy) {
	switch policy {
	case domain.FirstContactReplyRequestAccess:
		if client != nil && channelID != "" {
			_, _, _ = client.PostMessageContext(ctx, channelID,
				slack.MsgOptionText("Hi — this Nomi assistant isn't configured to talk to you yet. Ask the owner to add you to the allowlist.", false))
		}
	case domain.FirstContactQueueApproval:
		if p.identities != nil && userID != "" {
			_ = p.identities.Create(&domain.ChannelIdentity{
				PluginID:           PluginID,
				ConnectionID:       connID,
				ExternalIdentifier: userID,
				DisplayName:        userID,
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

func (p *Plugin) markConnectionStopped(connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.healthPerConn[connID]; ok {
		h.Running = false
	}
}

// Channel implements plugins.Channel for outbound Slack sends.
type Channel struct {
	connectionID string
	client       *slack.Client
}

// ConnectionID returns the Connection this Channel is scoped to.
func (c *Channel) ConnectionID() string { return c.connectionID }

// Kind identifies the channel family.
func (c *Channel) Kind() string { return "slack" }

// Send posts a message. externalConversationID is the
// "<channel_id>:<thread_ts>" tuple we encode in handleMessage. We split
// it back apart here so the reply lands in the correct thread.
//
// Routing rules (mirrors the Telegram plugin):
//  1. No attachments → PostMessageContext with msg.Text.
//  2. Attachments → UploadFileContext per attachment, with msg.Text
//     attached to the first upload via InitialComment so it shows
//     above the file in the thread. Subsequent attachments use their
//     own Caption (or nothing) as InitialComment.
//
// Slack's modern files.uploadV2 is a 3-call flow; for v1 we use the
// older UploadFileContext which slack-go still exposes and which
// handles inline bytes with one call. The v2 flow can replace this
// later without changing the Channel.Send contract.
func (c *Channel) Send(ctx context.Context, externalConversationID string, msg plugins.OutboundMessage) error {
	channelID, threadTS, err := splitExternalID(externalConversationID)
	if err != nil {
		return err
	}
	if len(msg.Attachments) == 0 {
		opts := []slack.MsgOption{slack.MsgOptionText(msg.Text, false)}
		if threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}
		_, _, err = c.client.PostMessageContext(ctx, channelID, opts...)
		return err
	}
	for i, att := range msg.Attachments {
		comment := ""
		if i == 0 && msg.Text != "" {
			comment = msg.Text
		} else if att.Caption != "" {
			comment = att.Caption
		}
		if err := c.uploadAttachment(ctx, channelID, threadTS, comment, att); err != nil {
			return err
		}
	}
	return nil
}

// uploadAttachment dispatches one Attachment via UploadFileContext.
// Inline bytes are wrapped in a bytes.Reader; URL-only attachments are
// rejected with a clear error because Slack's upload API doesn't
// accept remote URLs the way Telegram does — the caller must fetch
// the bytes first.
func (c *Channel) uploadAttachment(ctx context.Context, channelID, threadTS, comment string, att plugins.Attachment) error {
	if !att.IsInline() {
		return fmt.Errorf("slack channel: attachment without inline bytes is not supported (URL=%s)", att.URL)
	}
	filename := att.Filename
	if filename == "" {
		filename = slackFallbackFilename(att.Kind)
	}
	params := slack.UploadFileParameters{
		Reader:          bytes.NewReader(att.Data),
		Filename:        filename,
		Title:           filename,
		InitialComment:  comment,
		Channel:         channelID,
		ThreadTimestamp: threadTS,
		FileSize:        len(att.Data),
	}
	if _, err := c.client.UploadFileContext(ctx, params); err != nil {
		return fmt.Errorf("slack upload %q: %w", filename, err)
	}
	return nil
}

// slackFallbackFilename matches Telegram's helper: pick a sensible
// extension based on the attachment kind so Slack's preview renders
// correctly when the caller didn't supply one.
func slackFallbackFilename(kind plugins.AttachmentKind) string {
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

// handleInteraction resolves an approval when the user clicks one of
// the Block Kit buttons. Action ids are encoded as "nomi_approve:<id>"
// or "nomi_deny:<id>" so we can parse action + approval_id in one pass.
func (p *Plugin) handleInteraction(ctx context.Context, connID string, cb slack.InteractionCallback) {
	if p.approvals == nil {
		return
	}
	for _, action := range cb.ActionCallback.BlockActions {
		id := action.ActionID
		var approved bool
		var approvalID string
		switch {
		case strings.HasPrefix(id, "nomi_approve:"):
			approved = true
			approvalID = strings.TrimPrefix(id, "nomi_approve:")
		case strings.HasPrefix(id, "nomi_deny:"):
			approved = false
			approvalID = strings.TrimPrefix(id, "nomi_deny:")
		default:
			continue
		}
		if approvalID == "" {
			continue
		}
		if err := p.approvals.Resolve(ctx, approvalID, approved); err != nil {
			log.Printf("[slack plugin] resolve approval %s (connection %s): %v", approvalID, connID, err)
		}
	}
}

func splitExternalID(id string) (channelID, threadTS string, err error) {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return id[:i], id[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("malformed slack external conversation id %q (expected channel:thread_ts)", id)
}
