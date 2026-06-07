// Package obsidian implements the Obsidian Vault plugin — a tool +
// context-source plugin that lets assistants read and write notes in a
// user-selected vault folder. The plugin is plugin-less from Obsidian's
// perspective: Nomi reads/writes the .md files directly while Obsidian
// is running, and Obsidian's live-reload picks up the edits.
//
// This file is the scaffold (obsidian-01-vault): manifest, lifecycle,
// vault discovery + sandboxing helpers. The actual create/update/search
// tools land in obsidian-02-tools; vault-aware plan-time context lands
// in obsidian-03-context.
//
// The security story this plugin ships: no OAuth, no tokens, no
// network. Filesystem.read + filesystem.write are gated by the
// permission engine; the per-Connection vault path is the only thing
// the assistant can ever touch.
package obsidian

import (
	"context"
	"encoding/json"
	"sync"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/storage/db"
)

const PluginID = "com.nomi.obsidian"

const (
	configVaultPath   = "vault_path"
	configDisplayName = "display_name"
)

// Plugin is the Obsidian Vault plugin. Tool + context-source roles.
// One Connection = one vault. The context-source role arrives in
// obsidian-03; the tool role ships in obsidian-02.
type Plugin struct {
	connections *db.ConnectionRepository
	bindings    *db.AssistantBindingRepository

	mu      sync.RWMutex
	running bool
}

// NewPlugin constructs the Obsidian plugin. The bindings repo is used
// by tools to enforce per-assistant connection bindings (only an
// assistant explicitly bound to this vault may invoke its tools).
func NewPlugin(conns *db.ConnectionRepository, binds *db.AssistantBindingRepository) *Plugin {
	return &Plugin{connections: conns, bindings: binds}
}

// Manifest declares the Obsidian plugin's contract. Capabilities are
// scoped narrowly to filesystem.read + filesystem.write — explicitly
// no network.outgoing, which is the differentiator vs. cloud AI-notes
// services.
func (p *Plugin) Manifest() plugins.PluginManifest {
	return plugins.PluginManifest{
		ID:          PluginID,
		Name:        "Obsidian Vault",
		Version:     "0.1.0",
		Author:      "Nomi",
		Description: "Read, write, and search notes in a local Obsidian vault. No network access — runs entirely on your computer and only touches the folder you choose.",
		Cardinality: plugins.ConnectionMulti,
		Capabilities: []string{
			"filesystem.read",
			"filesystem.write",
		},
		Contributes: plugins.Contributions{
			Tools: p.toolContributions(),
			ContextSources: []plugins.ContextSourceContribution{
				{
					Name:        contextSourceName,
					Description: "Plan-time context surface that walks the connected vault, scores notes against the run's goal (filename, frontmatter tags, body), follows wikilinks one hop deep, and respects .obsidianignore. Returns a markdown blob the planner splices into its prompt.",
				},
			},
		},
		Requires: plugins.Requirements{
			ConfigSchema: map[string]plugins.ConfigField{
				configVaultPath: {
					Type:        "string",
					Label:       "Vault folder",
					Required:    true,
					Description: "Absolute path to your Obsidian vault folder. Nomi will only ever read or write files inside this folder. Pick the same folder Obsidian opens — the one containing your .md notes (and usually a .obsidian config directory).",
				},
				configDisplayName: {
					Type:        "string",
					Label:       "Display name",
					Required:    false,
					Description: "Optional label shown in the Connections tab when you have multiple vaults (e.g. \"Work\", \"Research\").",
				},
			},
			// No NetworkAllowlist: Obsidian is filesystem-only. The empty
			// list combined with no network.outgoing capability means the
			// permission engine will refuse any outbound request.
		},
	}
}

// Configure is a no-op; per-Connection vault config lives on the
// Connection rows themselves.
func (p *Plugin) Configure(context.Context, json.RawMessage) error { return nil }

// Start marks the plugin running. There is no background work in the
// scaffold; the fs.watch loop for live-reload arrives with the tools
// (obsidian-02) so the watcher has consumers to notify.
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

// Status reports plugin-level status.
func (p *Plugin) Status() plugins.PluginStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return plugins.PluginStatus{Running: p.running, Ready: true}
}

// VaultPathForConnection extracts the validated vault_path config field
// from a Connection. Returns an error if the field is missing, empty,
// or not a string. Used by every tool and context-source call so they
// converge on a single resolution point.
func VaultPathForConnection(conn *domain.Connection) (string, error) {
	if conn == nil {
		return "", ErrConnectionRequired
	}
	raw, ok := conn.Config[configVaultPath]
	if !ok {
		return "", ErrVaultPathMissing
	}
	s, ok := raw.(string)
	if !ok {
		return "", ErrVaultPathInvalidType
	}
	if s == "" {
		return "", ErrVaultPathMissing
	}
	return s, nil
}
