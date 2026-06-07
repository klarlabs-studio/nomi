package plugins

import (
	"context"
	"time"

	"go.klarlabs.de/nomi/internal/tools"
)

// ChannelProvider is implemented by plugins that act as conversational
// surfaces (Telegram, Email, Slack, Discord, …). Channels are stateful:
// each returned Channel is scoped to exactly one Connection and owns the
// outbound send path for that Connection.
//
// Implementations must return one Channel per enabled Connection. The
// registry invokes Channels() after Start() so the plugin has had a
// chance to build its per-Connection state.
type ChannelProvider interface {
	Plugin
	Channels() []Channel
}

// Channel is a single active conversational surface bound to a Connection.
// The runtime holds a Channel reference to deliver outbound messages
// (replies, approval prompts) via the same connection the inbound message
// came from. Channels don't need to handle inbound — plugins deliver
// inbound events to the runtime directly via the engine's
// CreateRunFromSource path.
type Channel interface {
	// ConnectionID identifies which Connection this Channel is scoped to.
	// Returned value is stable for the Channel's lifetime.
	ConnectionID() string

	// Kind is the channel family ("telegram", "email", …). Equal to the
	// matching ChannelContribution.Kind in the plugin's manifest.
	Kind() string

	// Send delivers a message (text + optional attachments) to a specific
	// external conversation (Telegram chat_id, email thread root
	// Message-ID, Slack channel ID). The underlying Connection is
	// implicit — each Channel instance is already tied to one.
	//
	// Plugins that don't yet handle attachments should honor msg.Text
	// and silently ignore msg.Attachments until their per-plugin media
	// task lands.
	Send(ctx context.Context, externalConversationID string, msg OutboundMessage) error
}

// SendText is a convenience for callers (and internal use by channel
// plugins themselves) that only need to send plain text. Constructs an
// OutboundMessage with no attachments and dispatches through Send.
func SendText(ctx context.Context, ch Channel, externalConversationID, text string) error {
	return ch.Send(ctx, externalConversationID, OutboundMessage{Text: text})
}

// OutboundMessage is the rich-media-aware payload passed to Channel.Send.
// Text is optional (channels that support attachment-only messages, like
// Telegram sendPhoto or Slack files.upload without text, accept empty);
// Attachments is optional (text-only replies use an empty slice).
type OutboundMessage struct {
	Text        string
	Attachments []Attachment
}

// AttachmentKind narrows the type so channel adapters can pick the right
// API surface (e.g. Telegram sendPhoto vs sendDocument). Kinds beyond
// this minimum set are safe to add — channels that don't recognize a
// kind fall back to treating it as a generic document.
type AttachmentKind string

const (
	AttachmentImage    AttachmentKind = "image"
	AttachmentDocument AttachmentKind = "document"
	AttachmentAudio    AttachmentKind = "audio" // voice notes, TTS output
	AttachmentVideo    AttachmentKind = "video"
)

// Attachment is one piece of attached media. Exactly one of Data or URL
// must be populated; Data is inline bytes (suitable for TTS output or
// small assistant-generated images), URL is a pre-uploaded reference
// (suitable for passing through inbound channel attachments without
// re-hosting).
//
// Filename + ContentType are optional but strongly recommended —
// channels without explicit content-type detection (Email MIME) fall
// back to application/octet-stream, which renders poorly.
//
// Caption is the text shown alongside the attachment on channels that
// support it (Telegram, Slack). Discord and Email render the caption
// as part of the message body instead.
type Attachment struct {
	Kind        AttachmentKind
	Filename    string
	ContentType string
	Data        []byte
	URL         string
	Caption     string
}

// IsInline reports whether this attachment carries bytes directly vs
// referencing an external URL. Channel adapters use this to pick the
// right upload strategy.
func (a Attachment) IsInline() bool { return len(a.Data) > 0 }

// ToolProvider is implemented by plugins that expose tools to the runtime.
// Tools are registered once per plugin and are stateless from the
// runtime's perspective; the Connection to act against is passed as an
// input parameter on each call. This keeps the existing tools.Registry /
// tools.Executor contract unchanged — plugins just contribute into the
// same pool.
type ToolProvider interface {
	Plugin
	Tools() []tools.Tool
}

// TriggerProvider is implemented by plugins that initiate runs from
// external events (new email, webhook, cron tick). Each Trigger is scoped
// to a Connection and runs as a background listener.
//
// The runtime routes TriggerEvents through the Scheduler (landed in a
// later task), which resolves the target assistant via the
// assistant_connection_bindings junction and creates the Run.
type TriggerProvider interface {
	Plugin
	Triggers() []Trigger
}

// Trigger is a background listener bound to a Connection. Start() is
// called by the runtime after all plugins are configured; Stop() is
// called during graceful shutdown. Fire the supplied callback to
// initiate a Run — the runtime handles assistant resolution and
// permission intersection.
type Trigger interface {
	ConnectionID() string
	// Kind is the trigger family ("inbox_watch", "webhook", "cron", …).
	// Matches the TriggerContribution.Kind in the plugin's manifest.
	Kind() string

	// Start begins listening. Fire events via onFire. Return any error
	// that prevents the trigger from running at all; transient errors
	// should be retried internally (like IMAP reconnection) rather than
	// bubbled out here.
	Start(ctx context.Context, onFire TriggerCallback) error

	// Stop gracefully shuts down the listener.
	Stop() error
}

// TriggerCallback is invoked by a Trigger to announce that an event
// occurred. The runtime decides whether to create a Run based on the
// event plus the assistant_connection_bindings that subscribe to it.
// An error return halts the trigger (e.g. "assistant no longer bound"
// is non-fatal; "credential rotated and invalid" is fatal).
type TriggerCallback func(ctx context.Context, event TriggerEvent) error

// TriggerEvent is the payload a Trigger fires into the runtime. The
// runtime uses ConnectionID to look up the assistant_connection_bindings
// where role="trigger"; the matching assistant (or primary if N) gets a
// Run with Goal as the goal text.
type TriggerEvent struct {
	// ConnectionID identifies the Connection this event came from. Fed
	// through assistant_connection_bindings to resolve the target
	// assistant.
	ConnectionID string

	// Kind echoes the Trigger.Kind() that fired this event. Useful for
	// analytics and debugging.
	Kind string

	// Goal is the text the runtime will create the Run with. Plugins
	// should render this to be human-readable ("New email from bob@…:
	// Re: Q3 planning") because it appears directly in the UI.
	Goal string

	// Metadata is event-specific context passed to the assistant as part
	// of the run's initial input. The planner may consume these during
	// step generation. Keys should be flat strings; nested structures go
	// through json.RawMessage values.
	Metadata map[string]interface{}
}

// ConnectionHealthReporter is an optional interface plugins implement to
// surface per-Connection health for the UI. The Plugins tab renders the
// returned ConnectionHealth as a badge: green dot when LastEventAt is
// recent, amber when stale, red when LastError is set.
//
// Plugins that don't implement this interface show a generic indicator
// derived from plugin-level PluginStatus instead.
type ConnectionHealthReporter interface {
	Plugin
	// ConnectionHealth returns the current health for a specific
	// Connection, or (zero value, false) if the plugin has no information
	// for that connection (e.g. it's disabled and never started).
	ConnectionHealth(connectionID string) (ConnectionHealth, bool)
}

// ConnectionHealth captures per-Connection runtime state the UI surfaces
// next to the connection row.
type ConnectionHealth struct {
	// Running reports whether the plugin has an active worker for this
	// Connection (poll goroutine, websocket session, etc).
	Running bool `json:"running"`
	// LastEventAt is the most recent time the plugin observed activity
	// on this connection (inbound message, successful poll tick, etc).
	// Zero-valued when the plugin has never seen activity yet.
	LastEventAt time.Time `json:"last_event_at,omitzero"`
	// LastError carries the most recent error message, or "" when the
	// last operation succeeded.
	LastError string `json:"last_error,omitempty"`
	// ErrorCount is the running count of errors since the last success.
	// Reset to zero when a subsequent operation succeeds.
	ErrorCount int `json:"error_count"`
}

// WebhookReceiver is implemented by plugins that accept inbound webhook
// payloads from external services (GitHub, Slack, etc). The Tunneled
// Inbound Receiver routes verified payloads to the matching plugin.
type WebhookReceiver interface {
	Plugin
	// ReceiveWebhook processes a verified webhook payload for a specific
	// connection. The plugin converts the payload into TriggerEvents and
	// fires them via onFire, or handles the payload directly if it doesn't
	// map to triggers (e.g. a health-check ping).
	ReceiveWebhook(ctx context.Context, connectionID string, payload []byte, headers map[string]string, onFire TriggerCallback) error
}

// ContextSourceProvider is implemented by plugins that expose read-only
// data surfaces queryable at plan time. Similar to today's folder-context
// tool, but tied to a Connection (e.g. a specific Obsidian vault or
// Google Drive account) rather than a filesystem path.
type ContextSourceProvider interface {
	Plugin
	ContextSources() []ContextSource
}

// ContextSource is one queryable data surface scoped to a Connection. The
// planner invokes Query at plan time; the result is concatenated into the
// plan prompt similarly to today's folder context.
type ContextSource interface {
	ConnectionID() string
	// Name matches the ContextSourceContribution.Name in the manifest.
	Name() string

	// Query returns context relevant to the request. Implementations
	// should respect ContextQueryRequest.MaxTokens (or a sensible
	// default when zero) and return the most relevant slice, not the
	// whole dataset. Errors are surfaced to the planner but do not
	// fail the run; the planner proceeds with whatever context
	// succeeded.
	Query(ctx context.Context, request ContextQueryRequest) (string, error)
}

// ContextQueryRequest is the parameter set passed to ContextSource.Query.
// Struct shape (rather than positional args) so future fields land
// without breaking every implementation — visibility caps, max tokens,
// run-time filters, etc.
type ContextQueryRequest struct {
	// RunID is the in-flight run requesting context. Sources that key
	// retrieval by run identity (Mnemos's rendered Context Block) need
	// this; sources that don't (folder context) ignore it.
	RunID string

	// Goal is the human-language goal text the planner is decomposing.
	// Used as a similarity / lexical query by sources that support
	// query-driven retrieval.
	Goal string

	// MaxTokens hints the upper bound on the returned string's token
	// budget. Zero means "implementation default." Honored as a hint;
	// implementations may truncate sooner if they have better signal.
	MaxTokens int
}
