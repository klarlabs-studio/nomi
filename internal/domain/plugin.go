package domain

import "time"

// Connection is one configured instance of a plugin — a Telegram bot,
// a Gmail account, a Slack workspace. See ADR 0001 §3.
//
// Credentials are never stored in this struct beyond their secret:// URIs;
// the plaintext values live in secrets.Store.
type Connection struct {
	ID             string            `json:"id"`
	PluginID       string            `json:"plugin_id"`
	Name           string            `json:"name"`
	Config         map[string]any    `json:"config"`          // non-secret plugin-specific settings
	CredentialRefs map[string]string `json:"credential_refs"` // logical-key → secret:// reference
	Enabled        bool              `json:"enabled"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`

	// WebhookConfig holds per-connection webhook settings for the
	// Tunneled Inbound Receiver. WebhookURL is the public HTTPS URL
	// the tunnel assigned to this connection; WebhookEnabled controls
	// whether the receiver accepts payloads for this connection;
	// WebhookEventAllowlist is a JSON array of event type strings
	// (e.g. ["push","pull_request"]) — empty means allow all.
	WebhookURL            string   `json:"webhook_url,omitempty"`
	WebhookEnabled        bool     `json:"webhook_enabled"`
	WebhookEventAllowlist []string `json:"webhook_event_allowlist"`
}

// BindingRole enumerates the roles an AssistantConnectionBinding can occupy.
// Matches the ADR 0001 role names exactly so JSON round-trips without
// translation.
type BindingRole string

const (
	BindingRoleChannel       BindingRole = "channel"
	BindingRoleTool          BindingRole = "tool"
	BindingRoleTrigger       BindingRole = "trigger"
	BindingRoleContextSource BindingRole = "context_source"
)

// IsValid reports whether the role is one of the four recognized values.
func (r BindingRole) IsValid() bool {
	switch r {
	case BindingRoleChannel, BindingRoleTool, BindingRoleTrigger, BindingRoleContextSource:
		return true
	}
	return false
}

// FirstContactPolicy enumerates how a channel plugin should react when an
// unknown sender reaches a connection — i.e. when no ChannelIdentity row
// matches the (plugin, connection, external_identifier) tuple. See ADR 0001 §9.
type FirstContactPolicy string

const (
	// FirstContactDrop silently ignores the message. Safest default; used
	// when the connection is still being configured or when the assistant
	// owner wants zero risk of a stranger reaching them.
	FirstContactDrop FirstContactPolicy = "drop"
	// FirstContactReplyRequestAccess sends a canned "please ask the owner
	// to add you to the allowlist" reply and drops the inbound. Useful for
	// email where silence would look like a delivery failure.
	FirstContactReplyRequestAccess FirstContactPolicy = "reply_request_access"
	// FirstContactQueueApproval creates an approval request for the
	// assistant owner to allow/deny the identity before the message is
	// processed. The inbound is held until the approval resolves.
	FirstContactQueueApproval FirstContactPolicy = "queue_approval"
)

// IsValid reports whether the policy is a recognized value.
func (p FirstContactPolicy) IsValid() bool {
	switch p {
	case FirstContactDrop, FirstContactReplyRequestAccess, FirstContactQueueApproval:
		return true
	}
	return false
}

// ChannelIdentity allowlists one external sender on a specific
// (plugin, connection). See ADR 0001 §9. The external_identifier is
// plugin-specific (phone / email / Slack user ID / Telegram user ID).
type ChannelIdentity struct {
	ID                 string    `json:"id"`
	PluginID           string    `json:"plugin_id"`
	ConnectionID       string    `json:"connection_id"`
	ExternalIdentifier string    `json:"external_identifier"`
	DisplayName        string    `json:"display_name,omitempty"`
	AllowedAssistants  []string  `json:"allowed_assistants,omitempty"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// PluginDistribution categorizes how a plugin reached the daemon.
// See ADR 0002 §1 for the lifecycle state model these compose into.
type PluginDistribution string

const (
	// PluginDistributionSystem is for plugins compiled into the nomid
	// binary. Always installed, never uninstallable.
	PluginDistributionSystem PluginDistribution = "system"
	// PluginDistributionMarketplace is for signed WASM bundles installed
	// from NomiHub or a custom URL. Installable + uninstallable +
	// updatable.
	PluginDistributionMarketplace PluginDistribution = "marketplace"
	// PluginDistributionDev is for unsigned WASM bundles loaded from
	// ~/.nomi/plugins-dev/. Off by default in production builds.
	PluginDistributionDev PluginDistribution = "dev"
)

// IsValid reports whether d is one of the recognized values.
func (d PluginDistribution) IsValid() bool {
	switch d {
	case PluginDistributionSystem, PluginDistributionMarketplace, PluginDistributionDev:
		return true
	}
	return false
}

// PluginState carries the per-plugin lifecycle state persisted in the
// plugin_state table. See ADR 0002 §1.
//
// AvailableVersion + LastCheckedAt are populated by the catalog poller
// (lifecycle-10); for now they ride along but stay zero-valued.
type PluginState struct {
	PluginID             string             `json:"plugin_id"`
	Distribution         PluginDistribution `json:"distribution"`
	Installed            bool               `json:"installed"`
	Enabled              bool               `json:"enabled"`
	EnabledRoles         []string           `json:"enabled_roles,omitempty"`
	Version              string             `json:"version,omitempty"`
	AvailableVersion     string             `json:"available_version,omitempty"`
	SourceURL            string             `json:"source_url,omitempty"`
	SignatureFingerprint string             `json:"signature_fingerprint,omitempty"`
	InstalledAt          time.Time          `json:"installed_at"`
	LastCheckedAt        *time.Time         `json:"last_checked_at,omitempty"`
}

// RunAttachment is one piece of media captured by a channel plugin
// during inbound message handling. The runtime's enrichment pass walks
// these rows and dispatches the right tool (transcribe, describe,
// extract) before planning starts. See ADR 0001 §rich-media.
type RunAttachment struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	Kind        string `json:"kind"` // "image" | "document" | "audio" | "video"
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	URL         string `json:"url,omitempty"`
	// ExternalID is the channel provider's stable reference (Telegram
	// file_id, Slack file_id, Discord attachment id). Stored so we can
	// re-fetch when URLs expire — Telegram's getFile URLs, in
	// particular, are short-lived.
	ExternalID string    `json:"external_id,omitempty"`
	SizeBytes  int64     `json:"size_bytes,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Conversation is a persistent thread tied to a specific (plugin, connection)
// pair. See ADR 0001 §8. One Connection hosts many Conversations (one per
// external user who messages the bot / emails the inbox / etc).
type Conversation struct {
	ID                     string    `json:"id"`
	PluginID               string    `json:"plugin_id"`
	ConnectionID           string    `json:"connection_id"`
	ExternalConversationID string    `json:"external_conversation_id"`
	IdentityID             string    `json:"identity_id,omitempty"`
	AssistantID            string    `json:"assistant_id"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// AssistantConnectionBinding expresses "this assistant uses this
// connection in this role." Reverses today's per-connection
// default_assistant_id: agents pick their connections, not the other
// way around. See ADR 0001 §4.
//
// The junction primary key is (AssistantID, ConnectionID, Role), so one
// (assistant, connection) pair can appear up to four times — once per
// role the plugin contributes and the agent opts into.
type AssistantConnectionBinding struct {
	AssistantID  string      `json:"assistant_id"`
	ConnectionID string      `json:"connection_id"`
	Role         BindingRole `json:"role"`
	Enabled      bool        `json:"enabled"`
	// IsPrimary disambiguates when an assistant has multiple bindings for
	// the same (plugin, role). Exactly one binding per (assistant, plugin,
	// role) may be primary; the repository enforces this invariant.
	IsPrimary bool      `json:"is_primary"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
}
