// Package store manages the on-disk layout for installed marketplace
// plugins. ADR 0002 §2 places these under ~/.nomi/plugins/<id>/ with a
// canonical layout that mirrors the .nomi-plugin bundle:
//
//	~/.nomi/plugins/com.example.foo/
//	  ├── manifest.json
//	  ├── plugin.wasm
//	  ├── README.md
//	  ├── publisher.json
//	  └── signature.ed25519
//
// Install writes atomically (tmp dir + rename) so a crash mid-install
// can never leave a half-populated plugin directory the boot path
// would try to load. Remove deletes the directory tree.
//
// Read access (WASM, Manifest, List) is the boot path's entry point —
// reload installed marketplace plugins on daemon start. The wasmplugin
// adapter consumes these to register modules into the plugin registry.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
)

// Store roots the on-disk layout. Constructed once at daemon boot and
// shared by the install handler + the boot reloader.
type Store struct {
	root string
}

// ErrNotInstalled is returned when a lookup targets a plugin id with
// no on-disk presence.
var ErrNotInstalled = errors.New("store: plugin not installed")

// New returns a Store that writes under root. The directory is created
// if missing; permission errors at construction surface here so the
// caller can fail fast on boot rather than discover the issue at first
// install.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("store: empty root")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", root, err)
	}
	return &Store{root: filepath.Clean(root)}, nil
}

// Root returns the absolute path the store writes under. Useful for
// log + audit output ("installed to /path/...").
func (s *Store) Root() string { return s.root }

// Install lays out a parsed Bundle on disk. Atomic via temp dir +
// rename: writes everything into <root>/.tmp-<id> then renames into
// <root>/<id>. If a plugin with the same id already exists, the call
// fails with a clear error so reinstall is an explicit Remove + Install
// sequence (which the install handler turns into a single API call).
//
// All file modes are 0o644 (data) / 0o755 (dir). signature and
// publisher files mirror the bundle exactly so uninstall + reinstall
// round-trips byte-for-byte.
func (s *Store) Install(b *bundle.Bundle) error {
	if b == nil || b.Manifest.ID == "" {
		return fmt.Errorf("store: bundle missing manifest.id")
	}
	id := b.Manifest.ID
	if !safePluginID(id) {
		return fmt.Errorf("store: unsafe plugin id %q", id)
	}
	final := filepath.Join(s.root, id)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("store: plugin %q already installed", id)
	}
	tmp := filepath.Join(s.root, ".tmp-"+id)
	// Clean any residue from a previous failed install attempt before
	// writing into the staging path.
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return fmt.Errorf("store: stage %s: %w", tmp, err)
	}

	files := map[string][]byte{
		"manifest.json":     b.RawManifest,
		"plugin.wasm":       b.WASM,
		"signature.ed25519": b.Signature,
	}
	if pubJSON, err := json.MarshalIndent(b.Publisher, "", "  "); err == nil {
		files["publisher.json"] = pubJSON
	} else {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("store: marshal publisher: %w", err)
	}
	if len(b.Readme) > 0 {
		files["README.md"] = b.Readme
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tmp, name), body, 0o600); err != nil {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("store: write %s: %w", name, err)
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("store: finalize %s: %w", final, err)
	}
	return nil
}

// Remove tears down a plugin directory. Idempotent: removing a
// non-installed plugin returns nil rather than ErrNotInstalled because
// uninstall callers rarely care about pre-state and a stale state row
// shouldn't block cleanup.
func (s *Store) Remove(pluginID string) error {
	if !safePluginID(pluginID) {
		return fmt.Errorf("store: unsafe plugin id %q", pluginID)
	}
	dir := filepath.Join(s.root, pluginID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("store: remove %s: %w", dir, err)
	}
	return nil
}

// WASM returns the plugin.wasm bytes for an installed plugin. Used by
// the boot reloader to instantiate marketplace plugins into the
// wasmhost on daemon start.
func (s *Store) WASM(pluginID string) ([]byte, error) {
	if !safePluginID(pluginID) {
		return nil, fmt.Errorf("store: unsafe plugin id %q", pluginID)
	}
	path := filepath.Join(s.root, pluginID, "plugin.wasm")
	body, err := os.ReadFile(path) //nolint:gosec // G304: path under the store root, plugin id validated by safePluginID
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotInstalled, pluginID)
	}
	return body, err
}

// Manifest returns the parsed manifest for an installed plugin.
func (s *Store) Manifest(pluginID string) (plugins.PluginManifest, error) {
	if !safePluginID(pluginID) {
		return plugins.PluginManifest{}, fmt.Errorf("store: unsafe plugin id %q", pluginID)
	}
	path := filepath.Join(s.root, pluginID, "manifest.json")
	body, err := os.ReadFile(path) //nolint:gosec // G304: path under the store root, plugin id validated by safePluginID
	if errors.Is(err, fs.ErrNotExist) {
		return plugins.PluginManifest{}, fmt.Errorf("%w: %s", ErrNotInstalled, pluginID)
	}
	if err != nil {
		return plugins.PluginManifest{}, err
	}
	var m plugins.PluginManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return plugins.PluginManifest{}, fmt.Errorf("store: parse manifest %s: %w", pluginID, err)
	}
	return m, nil
}

// List enumerates installed plugin IDs in stable order. Skips any
// directory that doesn't carry a parseable manifest (unlikely, but
// keeps boot resilient against a stray empty subdir).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", s.root, err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // skip .tmp-* and other hidden dirs
		}
		if !safePluginID(name) {
			continue
		}
		// Probe the manifest to confirm this is a real installation.
		if _, err := s.Manifest(name); err != nil {
			continue
		}
		ids = append(ids, name)
	}
	sort.Strings(ids)
	return ids, nil
}

// safePluginID rejects ids that would escape the root via path
// traversal or that contain characters our layout doesn't model. The
// install pathway never sees adversarial input today (manifests are
// parsed before reaching here) but future code paths may, so the gate
// is enforced at every disk-touching method.
func safePluginID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	if strings.ContainsAny(id, "/\\\x00") || strings.HasPrefix(id, ".") {
		return false
	}
	return true
}
