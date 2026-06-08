package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/integrations/google"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

// Plugin is the Gmail plugin. Tool-only, like Calendar — message sends
// are synchronous from the runtime's perspective, and inbound polling
// (label-watch, query-watch) is gmail-03's territory.
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository
	oauth       *google.OAuthManager
	secrets     secrets.Store

	// providerOverride lets tests inject a stub Provider without
	// going through the OAuth flow. Production builds leave this nil
	// and use providerFor (which constructs a GoogleProvider).
	providerOverride func(conn *domain.Connection) (Provider, error)

	mu      sync.RWMutex
	running bool
}

// NewPlugin constructs the Gmail plugin.
func NewPlugin(
	conns *db.ConnectionRepository,
	binds *db.AssistantBindingRepository,
	oauth *google.OAuthManager,
	secretStore secrets.Store,
) *Plugin {
	return &Plugin{
		connections: conns,
		bindings:    binds,
		oauth:       oauth,
		secrets:     secretStore,
	}
}

// SetProviderOverride is for tests — replaces providerFor with a
// caller-supplied factory so tools can be exercised against a mock
// without needing real OAuth credentials.
func (p *Plugin) SetProviderOverride(fn func(conn *domain.Connection) (Provider, error)) {
	p.providerOverride = fn
}

// Manifest declares the Gmail plugin's contract.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Gmail",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Send mail, search threads, manage labels via Gmail's REST API. Use this plugin (not the generic IMAP/SMTP one) when you need HTML drafts, label management, or thread-aware search.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"gmail.send",
			"gmail.read",
			"gmail.write",
			"network.outgoing",
		},
		Contributes: plugins.Contributions{
			Tools: []plugins.ToolContribution{
				{Name: "gmail.send", Capability: "gmail.send", RequiresConnection: true,
					Description: "Send a mail (or save a draft). Inputs: connection_id, to[], subject, body, cc?, bcc?, html?, attachments?[{filename, content_type?, data_base64}], thread_id?, draft?"},
				{Name: "gmail.search_threads", Capability: "gmail.read", RequiresConnection: true,
					Description: "Run a Gmail query (same syntax as the search box: from:, has:attachment, label:inbox, etc.). Inputs: connection_id, query, limit?"},
				{Name: "gmail.read_thread", Capability: "gmail.read", RequiresConnection: true,
					Description: "Fetch a thread including every message body. Inputs: connection_id, thread_id"},
				{Name: "gmail.label", Capability: "gmail.write", RequiresConnection: true,
					Description: "Add and/or remove labels on a message. Inputs: connection_id, message_id, add?[], remove?[]"},
				{Name: "gmail.archive", Capability: "gmail.write", RequiresConnection: true,
					Description: "Archive a message (removes the INBOX label). Inputs: connection_id, message_id"},
			},
			Triggers: []plugins.TriggerContribution{
				{Kind: TriggerKindFromWatch, EventType: "gmail.from_matched",
					Description: "Fire a run when a new message arrives from a configured sender. Rule shape: {kind: gmail.from_watch, name, from}."},
				{Kind: TriggerKindLabelWatch, EventType: "gmail.label_matched",
					Description: "Fire a run when an existing message gains a configured label. Rule shape: {kind: gmail.label_watch, name, label}. label is the Gmail label id (STARRED, INBOX, or Label_xxx)."},
				{Kind: TriggerKindQueryWatch, EventType: "gmail.query_matched",
					Description: "Fire a run when a new message matches a Gmail-search query. Rule shape: {kind: gmail.query_watch, name, query}."},
			},
		},
		Requires: plugins.Requirements{
			ConfigSchema: map[string]plugins.ConfigField{
				"provider": {
					Type: "string", Label: "Provider", Required: true, Default: "google",
					Description: `Gmail backend; only "google" in v1.`,
				},
				"account_id": {
					Type: "string", Label: "Account ID", Required: true,
					Description: "Google OAuth account id (from the device-flow linking step). Shared with the Calendar plugin — same account works for both.",
				},
				"client_id": {
					Type: "string", Label: "OAuth Client ID", Required: true,
					Description: "OAuth client id the account was linked under.",
				},
			},
			NetworkAllowlist: []string{"gmail.googleapis.com", "oauth2.googleapis.com"},
		},
	}
}

// Configure is a no-op — Gmail per-connection state lives on the
// Connection row and is read fresh on every tool call.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start marks the plugin running. No background work to launch.
func (p *Plugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = true
	return nil
}

// Stop unwinds the running flag.
func (p *Plugin) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	return nil
}

// Status returns plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// Tools implements plugins.ToolProvider.
func (p *Plugin) Tools() []tools.Tool {
	return []tools.Tool{
		&sendTool{base: base{plugin: p, name: "gmail.send", cap: "gmail.send"}},
		&searchThreadsTool{base: base{plugin: p, name: "gmail.search_threads", cap: "gmail.read"}},
		&readThreadTool{base: base{plugin: p, name: "gmail.read_thread", cap: "gmail.read"}},
		&labelTool{base: base{plugin: p, name: "gmail.label", cap: "gmail.write"}},
		&archiveTool{base: base{plugin: p, name: "gmail.archive", cap: "gmail.write"}},
	}
}

// providerFor maps a Connection to a Provider. Test path returns the
// override; production constructs a fresh GoogleProvider per call so
// the OAuth manager re-checks the access token (each call refreshes
// if needed — cheap because GetToken caches).
func (p *Plugin) providerFor(conn *domain.Connection) (Provider, error) {
	if p.providerOverride != nil {
		return p.providerOverride(conn)
	}
	providerKind, _ := conn.Config["provider"].(string)
	if providerKind == "" {
		providerKind = string(ProviderGoogle)
	}
	if !ProviderKind(providerKind).IsValid() {
		return nil, fmt.Errorf("unknown gmail provider %q", providerKind)
	}
	if p.oauth == nil {
		return nil, fmt.Errorf("google oauth manager not configured")
	}
	accountID, _ := conn.Config["account_id"].(string)
	clientID, _ := conn.Config["client_id"].(string)
	if accountID == "" || clientID == "" {
		return nil, fmt.Errorf("gmail requires account_id + client_id in connection config")
	}
	return NewGoogleProvider(p.oauth, clientID, accountID), nil
}

// --- tool boilerplate ---

type base struct {
	plugin *Plugin
	name   string
	cap    string
}

func (b *base) Name() string       { return b.name }
func (b *base) Capability() string { return b.cap }

// resolveProvider runs the binding + connection + provider lookups
// every Gmail tool needs. Mirrors the calendar plugin's helper so
// the failure surface is consistent.
func (b *base) resolveProvider(input map[string]interface{}) (Provider, error) {
	connectionID, _ := input["connection_id"].(string)
	if connectionID == "" {
		return nil, fmt.Errorf("%s: connection_id is required", b.name)
	}
	if assistantID, _ := input["__assistant_id"].(string); assistantID != "" && b.plugin.bindings != nil {
		ok, err := b.plugin.bindings.HasBinding(assistantID, connectionID, domain.BindingRoleTool)
		if err != nil {
			return nil, fmt.Errorf("%s: binding check failed: %w", b.name, err)
		}
		if !ok {
			return nil, plugins.ConnectionNotBoundError(assistantID, connectionID, PluginID)
		}
	}
	conn, err := b.plugin.connections.GetByID(connectionID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.name, err)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s: connection %s is disabled", b.name, connectionID)
	}
	provider, err := b.plugin.providerFor(conn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.name, err)
	}
	return provider, nil
}

// --- tools ---

type sendTool struct{ base }

func (t *sendTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	provider, err := t.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	opts, err := decodeSendOptions(input)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	res, err := provider.Send(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	return map[string]interface{}{
		"message_id": res.MessageID,
		"thread_id":  res.ThreadID,
		"is_draft":   res.IsDraft,
	}, nil
}

type searchThreadsTool struct{ base }

func (t *searchThreadsTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	provider, err := t.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	query, _ := input["query"].(string)
	limit := intFromInput(input, "limit", 25)
	threads, err := provider.SearchThreads(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	out := make([]map[string]interface{}, 0, len(threads))
	for _, th := range threads {
		out = append(out, map[string]interface{}{
			"id":      th.ID,
			"snippet": th.Snippet,
		})
	}
	return map[string]interface{}{"threads": out}, nil
}

type readThreadTool struct{ base }

func (t *readThreadTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	provider, err := t.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	threadID, _ := input["thread_id"].(string)
	if threadID == "" {
		return nil, fmt.Errorf("%s: thread_id is required", t.name)
	}
	th, err := provider.ReadThread(ctx, threadID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	msgs := make([]map[string]interface{}, 0, len(th.Messages))
	for _, m := range th.Messages {
		msgs = append(msgs, map[string]interface{}{
			"id":        m.ID,
			"thread_id": m.ThreadID,
			"from":      m.From,
			"to":        m.To,
			"cc":        m.Cc,
			"subject":   m.Subject,
			"date":      m.Date,
			"labels":    m.Labels,
			"body_text": m.BodyText,
			"body_html": m.BodyHTML,
		})
	}
	return map[string]interface{}{
		"id":       th.ID,
		"subject":  th.Subject,
		"messages": msgs,
	}, nil
}

type labelTool struct{ base }

func (t *labelTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	provider, err := t.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	messageID, _ := input["message_id"].(string)
	if messageID == "" {
		return nil, fmt.Errorf("%s: message_id is required", t.name)
	}
	add := stringSlice(input["add"])
	remove := stringSlice(input["remove"])
	if len(add) == 0 && len(remove) == 0 {
		return nil, fmt.Errorf("%s: at least one of add/remove is required", t.name)
	}
	if err := provider.Label(ctx, messageID, add, remove); err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	return map[string]interface{}{"status": "ok"}, nil
}

type archiveTool struct{ base }

func (t *archiveTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	provider, err := t.resolveProvider(input)
	if err != nil {
		return nil, err
	}
	messageID, _ := input["message_id"].(string)
	if messageID == "" {
		return nil, fmt.Errorf("%s: message_id is required", t.name)
	}
	if err := provider.Archive(ctx, messageID); err != nil {
		return nil, fmt.Errorf("%s: %w", t.name, err)
	}
	return map[string]interface{}{"status": "ok"}, nil
}

// --- input decoders ---

// decodeSendOptions translates the JSON-shaped tool input into
// SendOptions. Attachments arrive as base64-encoded `data_base64`
// fields because raw bytes don't survive the JSON tool envelope.
func decodeSendOptions(input map[string]interface{}) (SendOptions, error) {
	opts := SendOptions{
		To:       stringSlice(input["to"]),
		Cc:       stringSlice(input["cc"]),
		Bcc:      stringSlice(input["bcc"]),
		Subject:  stringFromInput(input, "subject"),
		Body:     stringFromInput(input, "body"),
		HTML:     stringFromInput(input, "html"),
		ThreadID: stringFromInput(input, "thread_id"),
	}
	if v, ok := input["draft"].(bool); ok {
		opts.Draft = v
	}
	if len(opts.To) == 0 {
		return opts, fmt.Errorf("to is required (string or string array)")
	}
	rawAttachments, _ := input["attachments"].([]interface{})
	for i, raw := range rawAttachments {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return opts, fmt.Errorf("attachments[%d]: expected object", i)
		}
		filename, _ := m["filename"].(string)
		if filename == "" {
			return opts, fmt.Errorf("attachments[%d]: filename is required", i)
		}
		dataB64, _ := m["data_base64"].(string)
		bytes, err := base64.StdEncoding.DecodeString(dataB64)
		if err != nil {
			return opts, fmt.Errorf("attachments[%d]: data_base64 invalid: %w", i, err)
		}
		ct, _ := m["content_type"].(string)
		cid, _ := m["content_id"].(string)
		opts.Attachments = append(opts.Attachments, Attachment{
			Filename:    filename,
			ContentType: ct,
			ContentID:   cid,
			Data:        bytes,
		})
	}
	return opts, nil
}

func stringFromInput(input map[string]interface{}, key string) string {
	v, _ := input[key].(string)
	return v
}

func intFromInput(input map[string]interface{}, key string, def int) int {
	switch v := input[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

// stringSlice accepts either a single string or an array of strings.
// JSON-unmarshaled values arrive as []interface{} so we have to
// case-split — purely a serialization quirk, not Gmail-specific.
func stringSlice(raw interface{}) []string {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	}
	return nil
}
