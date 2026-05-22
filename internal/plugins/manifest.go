// Package plugins defines Nomi's plugin contract: one unified abstraction
// for every external integration (Telegram, Email, Slack, Gmail, GitHub,
// Obsidian, …). A Plugin declares the roles it plays — channel, tool,
// trigger, context source — via optional Go interfaces (see roles.go) and
// the host wires each contribution into the right subsystem.
//
// The design rationale and alternatives are captured in
// docs/adr/0001-plugin-architecture.md. This package intentionally contains
// only types and interfaces; the registry, role-view projections, and
// concrete plugin implementations live in sibling packages.
package plugins

import (
	"encoding/json"
	"time"
)

// PluginManifest describes a plugin to the host and to the UI. It is the
// only source of truth about what a plugin can do — the runtime consults
// it when intersecting capabilities, when listing plugins in the UI, and
// when validating an assistant's connection bindings.
type PluginManifest struct {
	// ID is stable, reverse-DNS (e.g. "com.nomi.telegram"). Used as the
	// foreign key from plugin_connections rows and assistant bindings.
	ID string `json:"id"`
	// Name is the human-readable display name.
	Name string `json:"name"`
	// Version is the plugin's own semver; independent of the nomid binary.
	Version string `json:"version"`
	// Author is optional — "Nomi" for built-ins, third-party name for
	// marketplace plugins.
	Author string `json:"author,omitempty"`
	// Description is a one-paragraph overview surfaced in the Plugins tab.
	Description string `json:"description,omitempty"`
	// IconURL is optional; a relative path is resolved against the plugin's
	// bundle root (once out-of-process plugins exist). For in-tree plugins
	// this is typically empty and the UI falls back to a role-derived icon.
	IconURL string `json:"icon_url,omitempty"`
	// Cardinality declares how many Connections this plugin supports.
	// See ConnectionCardinality for semantics.
	Cardinality ConnectionCardinality `json:"cardinality"`
	// Capabilities is the exhaustive set of capability strings the plugin
	// may ever request or provide. Acts as a ceiling: the runtime refuses
	// any capability not declared here. Permission engine gating still
	// applies on top of this — declaring capability doesn't grant it.
	Capabilities []string `json:"capabilities"`
	// Contributes enumerates which role-specific contributions this plugin
	// exposes. The shape of each contribution mirrors the role's runtime
	// interface (see roles.go).
	Contributes Contributions `json:"contributes"`
	// Requires declares what the plugin needs to operate: credentials
	// (stored via secrets.Store) and user-facing configuration fields.
	Requires Requirements `json:"requires,omitempty"`
}

// ConnectionCardinality declares how many Connections a plugin accepts.
//
// The user-facing rule-of-thumb: "single" is for system plugins that have
// no external account concept (filesystem, shell), "multi" is the default
// for anything with an account/bot/workspace (Telegram, Gmail, Slack).
// "multi-multi" is reserved for plugins where a single logical Connection
// has its own sub-entities (a Slack workspace containing multiple channel
// selections, for example). No v1 plugin uses multi-multi; it is included
// so the UI doesn't need to be re-modelled when we reach that case.
type ConnectionCardinality string

const (
	ConnectionSingle     ConnectionCardinality = "single"
	ConnectionMulti      ConnectionCardinality = "multi"
	ConnectionMultiMulti ConnectionCardinality = "multi-multi"
)

// Contributions enumerates which roles a plugin plays. Zero-valued fields
// mean the plugin does not contribute that role. At least one contribution
// is required for a plugin to be useful; the registry enforces this at
// Register time.
type Contributions struct {
	Channels       []ChannelContribution       `json:"channels,omitempty"`
	Tools          []ToolContribution          `json:"tools,omitempty"`
	Triggers       []TriggerContribution       `json:"triggers,omitempty"`
	ContextSources []ContextSourceContribution `json:"context_sources,omitempty"`
}

// HasRole reports whether this plugin contributes the given role. Uses
// string role names ("channel", "tool", "trigger", "context_source") so
// the UI can filter without hard-coding role enums.
func (c Contributions) HasRole(role string) bool {
	switch role {
	case "channel":
		return len(c.Channels) > 0
	case "tool":
		return len(c.Tools) > 0
	case "trigger":
		return len(c.Triggers) > 0
	case "context_source":
		return len(c.ContextSources) > 0
	default:
		return false
	}
}

// ChannelContribution describes a conversational surface the plugin exposes.
// The actual runtime Channel instances are obtained via ChannelProvider.Channels(),
// one per enabled Connection; the contribution here is the static metadata.
type ChannelContribution struct {
	// Kind is the channel family (e.g. "telegram", "email", "slack").
	// Used by the runtime to route inbound events to the right plugin.
	Kind string `json:"kind"`
	// Description is surfaced in the Plugins tab and Assistant composer.
	Description string `json:"description,omitempty"`
	// SupportsThreading reports whether this channel persists multi-turn
	// state via the Conversation entity (most do; SMS-style one-off is rare).
	SupportsThreading bool `json:"supports_threading"`
}

// ToolContribution describes one tool the plugin exposes. Tools are
// registered into the existing tools.Registry via the ToolProvider
// interface. The Capability string must appear in the plugin's
// Capabilities list; the registry validates this at plugin registration.
type ToolContribution struct {
	// Name is the fully-qualified tool name (e.g. "gmail.send"). Must be
	// unique across all registered plugins.
	Name string `json:"name"`
	// Capability is the permission string gating this tool. Typically
	// matches Name but can diverge (e.g. filesystem.context uses
	// filesystem.read as its capability).
	Capability string `json:"capability"`
	// Description tells the LLM what this tool does; appears in tool
	// schemas passed to the planner.
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON Schema for the tool's input. Optional — tools
	// may accept arbitrary map[string]interface{} today, but specifying a
	// schema unlocks LLM-side structured output validation.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// RequiresConnection reports whether calls to this tool must specify a
	// connection_id (true for Gmail, Slack, etc.; false for stateless
	// system tools like filesystem.read).
	RequiresConnection bool `json:"requires_connection"`
}

// TriggerContribution describes one trigger kind the plugin exposes.
// Instances are materialized via TriggerProvider.Triggers(), one per
// configured trigger (e.g. per inbox-watch rule per Connection).
type TriggerContribution struct {
	// Kind is the trigger family (e.g. "inbox_watch", "webhook", "cron",
	// "pre_meeting"). Distinct from EventType which is the specific event
	// the trigger fires on.
	Kind string `json:"kind"`
	// EventType is the canonical event name emitted when the trigger fires
	// (e.g. "gmail.message_received", "github.pr_opened").
	EventType string `json:"event_type"`
	// Description explains what this trigger reacts to.
	Description string `json:"description,omitempty"`
}

// ContextSourceContribution describes a read-only data surface queryable
// at plan time (similar to today's folder-context tool but plugin-aware).
type ContextSourceContribution struct {
	// Name is the context source identifier (e.g. "obsidian.vault").
	Name string `json:"name"`
	// Description tells the planner what kind of context this surface
	// provides.
	Description string `json:"description,omitempty"`
}

// Requirements describe what a plugin needs to operate. The runtime
// refuses to Start() a plugin until all required credentials and config
// fields are satisfied for every enabled Connection.
type Requirements struct {
	// Credentials are secrets the plugin needs (bot tokens, API keys,
	// OAuth refresh tokens). Values land in secrets.Store; the plugin
	// receives only secret:// references in its Configure input.
	Credentials []CredentialSpec `json:"credentials,omitempty"`
	// ConfigSchema describes non-secret per-Connection fields the UI
	// should render (host, port, polling interval, etc.).
	ConfigSchema map[string]ConfigField `json:"config_schema,omitempty"`
	// NetworkAllowlist enumerates the host patterns the plugin will
	// reach over network.outgoing. Wildcard syntax matches the
	// permissions engine: `*.slack.com` is leading-dot anchored and
	// does NOT match `slack.com.attacker.com`. For bundled (system)
	// plugins this is informational + surfaces in the install dialog;
	// for marketplace WASM plugins it is enforced at the
	// host_http_request boundary (intersected with the user policy's
	// allowed_hosts constraint). Empty means "no fixed hosts" — for
	// per-connection cases like email IMAP/SMTP, leave this empty and
	// let the runtime supplement allowed hosts from connection config.
	NetworkAllowlist []string `json:"network_allowlist,omitempty"`
}

// CredentialSpec declares one credential the plugin requires. The Kind
// field drives the UI: "oauth_google" pops a device-flow dialog,
// "bot_token" pops a password input, "imap_password" pops an app-password
// wizard with provider presets. Adding new Kinds is a coordinated
// frontend + backend change.
type CredentialSpec struct {
	Kind        string `json:"kind"`
	Key         string `json:"key"`
	Label       string `json:"label"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// ConfigField describes a single user-facing, non-secret config field.
// Shape mirrors today's connectors.ConfigField for UX continuity.
//
// Type "enum" turns the field into a fixed-choice dropdown in the UI;
// the Options slice supplies the valid values. Free-text Types
// (string/number/boolean) ignore Options.
type ConfigField struct {
	Type        string         `json:"type"` // "string" | "boolean" | "number" | "enum"
	Label       string         `json:"label"`
	Required    bool           `json:"required"`
	Default     string         `json:"default,omitempty"`
	Description string         `json:"description,omitempty"`
	Options     []ConfigOption `json:"options,omitempty"`
}

// ConfigOption is one entry in an enum ConfigField's choice list. Label
// falls back to Value when omitted so manifests stay terse for the
// common case where the value IS the label.
type ConfigOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// PluginStatus is the runtime state the Plugins tab surfaces per plugin.
// Per-Connection status lives on the Connection row and is not duplicated
// here.
type PluginStatus struct {
	Running     bool      `json:"running"`
	Ready       bool      `json:"ready"`
	LastError   string    `json:"last_error,omitempty"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`
}
