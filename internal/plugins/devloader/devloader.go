// Package devloader scans a directory of unsigned .nomi-plugin
// bundles and loads them into the wasmhost — the local plugin
// authoring path described in ADR 0002 §2 (dev tier) and lifecycle-08.
//
// Three guardrails distinguish dev loads from marketplace installs:
//
//  1. Off by default: the daemon only calls Load when the
//     `dev_plugins_enabled` setting is true. Flipping the setting back
//     to false does not unload — restart is required.
//  2. No signature verification. Dev bundles are unsigned by design;
//     the UI surfaces this with a red banner via
//     PluginState.Distribution = "dev".
//  3. Errors are per-file, not fatal. One broken bundle in the dev
//     dir does not prevent the others from loading; the boot path
//     logs each failure and moves on.
package devloader

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/plugins/wasmplugin"
)

// Result is one loaded dev plugin paired with the source file path so
// the boot log + plugin_state row can name where it came from.
// Failed loads are returned via the separate Errors slice on
// LoadResult so partial success is observable to the caller.
type Result struct {
	Path     string
	Plugin   *wasmplugin.Plugin
	Manifest plugins.PluginManifest
}

// LoadResult bundles successful loads with per-file errors. Callers
// log Errors at WARN level (one bad bundle shouldn't quiet the others)
// and register Plugins into the runtime registry.
type LoadResult struct {
	Plugins []Result
	Errors  []error
}

// Load scans dir for files matching *.nomi-plugin or *.wasm, parses
// each as a bundle, and instantiates a wasmplugin.Plugin per success.
// dir is created if missing — the boot path can call Load
// unconditionally without first probing for the directory.
//
// Files are processed in lexical order so the boot log is stable
// across restarts.
func Load(ctx context.Context, dir string, loader *wasmhost.Loader) (*LoadResult, error) {
	if loader == nil {
		return nil, errors.New("devloader: nil loader")
	}
	if dir == "" {
		return nil, errors.New("devloader: empty directory")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("devloader: mkdir %s: %w", dir, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("devloader: read %s: %w", dir, err)
	}

	out := &LoadResult{}
	files := pickBundleFiles(entries)
	sort.Strings(files)

	for _, name := range files {
		path := filepath.Join(dir, name)
		res, err := loadOne(ctx, path, loader)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Errorf("%s: %w", name, err))
			continue
		}
		out.Plugins = append(out.Plugins, res)
	}
	return out, nil
}

// loadOne parses + verifies-structurally + instantiates one bundle.
// Skips signature checks deliberately (dev bundles are unsigned).
func loadOne(ctx context.Context, path string, loader *wasmhost.Loader) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()

	b, err := bundle.Open(f)
	if err != nil {
		return Result{}, fmt.Errorf("bundle: %w", err)
	}
	mod, err := loader.Load(ctx, b.Manifest.ID, b.WASM)
	if err != nil {
		return Result{}, fmt.Errorf("wasm: %w", err)
	}
	return Result{
		Path:     path,
		Plugin:   wasmplugin.New(b.Manifest, mod, nil),
		Manifest: b.Manifest,
	}, nil
}

// pickBundleFiles filters fs.DirEntry down to plain files whose name
// looks like a bundle. *.nomi-plugin is the only accepted extension —
// raw .wasm files lack the manifest the loader needs to know what
// tools the module exposes. Plugin authors iterating quickly should
// repack with `tar -czf foo.nomi-plugin manifest.json plugin.wasm
// publisher.json signature.ed25519` (any bytes work for signature in
// dev — verification is skipped).
func pickBundleFiles(entries []fs.DirEntry) []string {
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".nomi-plugin") {
			out = append(out, name)
		}
	}
	return out
}
