// Package update implements the plugin update flow described by ADR
// 0002 §2 / lifecycle-10. Two operations:
//
//	Scan    — diff installed plugins against the NomiHub catalog.
//	          Bumps plugin_state.available_version + last_checked_at
//	          and emits plugin.update_available when the catalog ships
//	          a higher semver than the installed one. No bundle
//	          download. Cheap; safe to run on a daily ticker.
//	Update  — synchronous "do it now" path: fetches the catalog
//	          entry's bundle, runs full signing.Verify, drains the
//	          live module, swaps in the new module + bundle on disk,
//	          and bumps plugin_state.version. Emits plugin.updated.
//	          Explicit only — no caller-less invocation path. ADR
//	          guarantees: silent updates would surprise users.
//
// Both operations are conservative on failure: a corrupt catalog
// entry, a signature mismatch, or a wasmhost.Load error rolls back
// to the pre-update state and surfaces the cause via wrapped errors.
package update

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/events"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/plugins/signing"
	"go.klarlabs.de/nomi/internal/plugins/store"
	"go.klarlabs.de/nomi/internal/plugins/wasmhost"
	"go.klarlabs.de/nomi/internal/plugins/wasmplugin"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// Sentinel errors so the API handler can map causes to status codes.
var (
	ErrPluginNotInstalled  = errors.New("update: plugin not installed")
	ErrNoCatalogEntry      = errors.New("update: no catalog entry for plugin")
	ErrAlreadyAtLatest     = errors.New("update: already at latest version")
	ErrBundleHashMismatch  = errors.New("update: bundle SHA-256 doesn't match catalog entry")
	ErrSystemPluginRefused = errors.New("update: system plugins do not update via this path")
)

// Deps bundles the collaborators Update + Scan need. Constructed once
// at boot; Update is called per-request, Scan on the poll ticker.
type Deps struct {
	Registry *plugins.Registry
	State    *db.PluginStateRepository
	Store    *store.Store
	Verifier *signing.Verifier
	Loader   *wasmhost.Loader
	Bus      *events.EventBus
	// Catalog returns the most recent verified catalog. Same provider
	// the marketplace endpoint uses (they share the cache).
	Catalog func(ctx context.Context) (*hub.Catalog, error)
	// HTTPFetch downloads bundle bytes from a catalog entry's
	// BundleURL. Defaults to the install handler's defaultHTTPFetch
	// — exposed here so tests can stub.
	HTTPFetch func(ctx context.Context, url string) ([]byte, error)
	// Now is the clock used for last_checked_at + log messages. nil
	// means time.Now. Tests inject a frozen clock.
	Now func() time.Time
}

// Scan walks every installed marketplace plugin, compares the
// installed version to the catalog's latest_version, and persists
// available_version + last_checked_at + emits plugin.update_available
// when a newer version is published. Idempotent: re-running with the
// same inputs is a no-op.
//
// Returns the number of plugins newly flagged with updates so the
// caller (poller log line, scheduled-job report) can surface activity
// without subscribing to the event bus.
func Scan(ctx context.Context, deps Deps) (int, error) {
	if deps.State == nil || deps.Catalog == nil {
		return 0, errors.New("update: Scan requires State + Catalog")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	cat, err := deps.Catalog(ctx)
	if err != nil {
		return 0, fmt.Errorf("update: fetch catalog: %w", err)
	}
	byID := make(map[string]hub.Entry, len(cat.Entries))
	for _, e := range cat.Entries {
		byID[e.PluginID] = e
	}

	all, err := deps.State.List()
	if err != nil {
		return 0, fmt.Errorf("update: list plugin_state: %w", err)
	}
	checkedAt := now().UTC()
	flagged := 0
	for _, st := range all {
		if st.Distribution != domain.PluginDistributionMarketplace || !st.Installed {
			continue
		}
		entry, ok := byID[st.PluginID]
		if !ok {
			// Plugin no longer in the catalog — leave the row alone.
			// A future delisting flow can decide what to do here.
			continue
		}
		st.LastCheckedAt = &checkedAt
		// Only set AvailableVersion when the catalog actually ships
		// something newer. Equal/older versions clear the field so
		// stale "update available" indicators don't linger after an
		// update.
		newer := compareSemver(entry.LatestVersion, st.Version) > 0
		previousAvailable := st.AvailableVersion
		if newer {
			st.AvailableVersion = entry.LatestVersion
		} else {
			st.AvailableVersion = ""
		}
		if err := deps.State.Upsert(st); err != nil {
			return flagged, fmt.Errorf("update: upsert state for %s: %w", st.PluginID, err)
		}
		if newer && previousAvailable != entry.LatestVersion && deps.Bus != nil {
			_, _ = deps.Bus.Publish(ctx, domain.EventPluginUpdateAvailable, st.PluginID, nil, map[string]interface{}{
				"plugin_id":    st.PluginID,
				"from_version": st.Version,
				"to_version":   entry.LatestVersion,
				"bundle_url":   entry.BundleURL,
			})
			flagged++
		}
	}
	return flagged, nil
}

// Update is the user-triggered "install the new version now" path.
// Returns the new PluginState on success. Caller responsibilities:
// authorize the request, surface the wrapped error in the response.
//
// Algorithm:
//
//  1. Look up plugin_state — abort if not installed or system tier.
//  2. Find catalog entry — abort if delisted.
//  3. Compare versions — return ErrAlreadyAtLatest if not newer.
//  4. Fetch bundle bytes — verify SHA-256 matches catalog entry,
//     parse with bundle.Open, run signing.Verify.
//  5. Compile new wasm module ahead of swap (fail-fast before we
//     touch the live one).
//  6. Stop + Unregister the old plugin.
//  7. store.Remove old bundle + store.Install new one.
//  8. Register new plugin into the registry.
//  9. Upsert plugin_state with version = new, available_version = "".
//  10. Emit plugin.updated.
//
// Step 5 fails before any user-visible state change happens — that
// keeps the "update aborted, prior version preserved" guarantee the
// task description requires.
func Update(ctx context.Context, deps Deps, pluginID string) (*domain.PluginState, error) {
	if deps.State == nil || deps.Store == nil || deps.Verifier == nil || deps.Loader == nil || deps.Registry == nil || deps.Catalog == nil || deps.HTTPFetch == nil {
		return nil, errors.New("update: Update missing required dependencies")
	}

	st, err := deps.State.Get(pluginID)
	if err != nil || st == nil || !st.Installed {
		return nil, fmt.Errorf("%w: %s", ErrPluginNotInstalled, pluginID)
	}
	if st.Distribution == domain.PluginDistributionSystem {
		return nil, ErrSystemPluginRefused
	}

	cat, err := deps.Catalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("update: fetch catalog: %w", err)
	}
	var entry *hub.Entry
	for i := range cat.Entries {
		if cat.Entries[i].PluginID == pluginID {
			entry = &cat.Entries[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoCatalogEntry, pluginID)
	}
	if compareSemver(entry.LatestVersion, st.Version) <= 0 {
		return nil, ErrAlreadyAtLatest
	}

	body, err := deps.HTTPFetch(ctx, entry.BundleURL)
	if err != nil {
		return nil, fmt.Errorf("update: download %s: %w", entry.BundleURL, err)
	}
	b, err := bundle.Open(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("update: parse bundle: %w", err)
	}
	if entry.SHA256 != "" && b.Hash != entry.SHA256 {
		return nil, fmt.Errorf("%w: catalog=%s bundle=%s", ErrBundleHashMismatch, entry.SHA256, b.Hash)
	}
	if b.Manifest.ID != pluginID {
		return nil, fmt.Errorf("update: bundle id %q != target %q", b.Manifest.ID, pluginID)
	}
	if err := deps.Verifier.Verify(b); err != nil {
		return nil, fmt.Errorf("update: verify: %w", err)
	}

	// Pre-compile the new module BEFORE touching the running one. If
	// this fails we still have the old version live + on disk.
	newMod, err := deps.Loader.Load(ctx, pluginID+"@"+b.Manifest.Version, b.WASM)
	if err != nil {
		return nil, fmt.Errorf("update: load new wasm: %w", err)
	}

	// At this point the new module is good. Swap the world.
	if oldPlug, err := deps.Registry.Get(pluginID); err == nil {
		_ = oldPlug.Stop()
		_ = deps.Registry.Unregister(pluginID)
	}
	if err := deps.Store.Remove(pluginID); err != nil {
		_ = newMod.Close(ctx)
		return nil, fmt.Errorf("update: remove old bundle: %w", err)
	}
	if err := deps.Store.Install(b); err != nil {
		_ = newMod.Close(ctx)
		// At this point the old plugin is unregistered + removed and
		// the new one couldn't be persisted. Mark state as
		// installed=false so the user sees "needs reinstall" rather
		// than a half-state.
		st.Installed = false
		_ = deps.State.Upsert(st)
		return nil, fmt.Errorf("update: install new bundle: %w", err)
	}
	newPlug := wasmplugin.New(b.Manifest, newMod, nil)
	if err := deps.Registry.Register(newPlug); err != nil {
		_ = newMod.Close(ctx)
		_ = deps.Store.Remove(pluginID)
		st.Installed = false
		_ = deps.State.Upsert(st)
		return nil, fmt.Errorf("update: register: %w", err)
	}

	fromVersion := st.Version
	st.Version = b.Manifest.Version
	st.AvailableVersion = ""
	st.SignatureFingerprint = b.Publisher.KeyFingerprint
	if err := deps.State.Upsert(st); err != nil {
		return nil, fmt.Errorf("update: upsert post-swap state: %w", err)
	}

	if deps.Bus != nil {
		_, _ = deps.Bus.Publish(ctx, domain.EventPluginUpdated, pluginID, nil, map[string]interface{}{
			"plugin_id":    pluginID,
			"from_version": fromVersion,
			"to_version":   st.Version,
		})
	}
	return st, nil
}

// compareSemver returns +1 if a > b, 0 if equal, -1 if a < b.
// Three-segment "MAJOR.MINOR.PATCH" only — pre-release suffixes
// (`-rc.1`, etc.) are treated as plain strings via lex compare on
// the trailing segment. Sufficient for v1; a real semver lib drops
// in if/when we need pre-release ordering.
func compareSemver(a, b string) int {
	if a == b {
		return 0
	}
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		va := segOrZero(pa, i)
		vb := segOrZero(pb, i)
		ai, aErr := strconv.Atoi(va)
		bi, bErr := strconv.Atoi(vb)
		if aErr != nil || bErr != nil {
			// One side is non-numeric (likely a pre-release tag).
			// Lexicographic fallback keeps comparison total.
			switch {
			case va > vb:
				return 1
			case va < vb:
				return -1
			default:
				continue
			}
		}
		switch {
		case ai > bi:
			return 1
		case ai < bi:
			return -1
		}
	}
	return 0
}

func segOrZero(parts []string, i int) string {
	if i >= len(parts) {
		return "0"
	}
	return parts[i]
}
