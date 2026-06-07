package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
)

// PluginStateRepository handles CRUD for plugin_state. See ADR 0002 §1.
type PluginStateRepository struct {
	db *DB
}

// NewPluginStateRepository constructs a repository.
func NewPluginStateRepository(db *DB) *PluginStateRepository {
	return &PluginStateRepository{db: db}
}

// Get returns the current state for a plugin or sql.ErrNoRows if
// the plugin has never been seeded.
func (r *PluginStateRepository) Get(pluginID string) (*domain.PluginState, error) {
	row := r.db.QueryRow(
		`SELECT plugin_id, distribution, installed, enabled, version,
		        available_version, source_url, signature_fingerprint,
		        installed_at, last_checked_at
		 FROM plugin_state WHERE plugin_id = ?`,
		pluginID,
	)
	return scanPluginState(row)
}

// List returns every plugin_state row, ordered by installed_at so
// system plugins (seeded first at boot) lead the list.
func (r *PluginStateRepository) List() ([]*domain.PluginState, error) {
	rows, err := r.db.Query(
		`SELECT plugin_id, distribution, installed, enabled, version,
		        available_version, source_url, signature_fingerprint,
		        installed_at, last_checked_at
		 FROM plugin_state ORDER BY installed_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.PluginState
	for rows.Next() {
		s, err := scanPluginState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// EnsureSystemPlugin seeds a row for a system plugin at boot. Idempotent:
// safe to call on every daemon start. Existing rows are not overwritten —
// the user's enabled/disabled choice survives across restarts.
func (r *PluginStateRepository) EnsureSystemPlugin(pluginID, version string) error {
	if pluginID == "" {
		return fmt.Errorf("plugin_id required")
	}
	_, err := r.db.Exec(
		`INSERT INTO plugin_state (plugin_id, distribution, installed, enabled, version, installed_at)
		 VALUES (?, ?, 1, 1, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(plugin_id) DO UPDATE SET
		   distribution = excluded.distribution,
		   version      = excluded.version`,
		pluginID, string(domain.PluginDistributionSystem), version,
	)
	return err
}

// SetEnabled flips the enabled bit. Returns sql.ErrNoRows if no row
// exists for the plugin (caller should EnsureSystemPlugin or install
// first).
func (r *PluginStateRepository) SetEnabled(pluginID string, enabled bool) error {
	res, err := r.db.Exec(
		`UPDATE plugin_state SET enabled = ? WHERE plugin_id = ?`,
		boolToInt(enabled), pluginID,
	)
	if err != nil {
		return fmt.Errorf("update enabled: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetEnabledRoles replaces the enabled_roles slice. Pass nil to
// clear the override and fall back to "all roles enabled".
func (r *PluginStateRepository) SetEnabledRoles(pluginID string, roles []string) error {
	enc, err := json.Marshal(roles)
	if err != nil {
		return fmt.Errorf("marshal enabled_roles: %w", err)
	}
	res, err := r.db.Exec(
		`UPDATE plugin_state SET enabled_roles = ? WHERE plugin_id = ?`,
		string(enc), pluginID,
	)
	if err != nil {
		return fmt.Errorf("update enabled_roles: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// IsEnabled is the hot-path lookup used by pluginRegistry.StartAll. A
// missing row returns true so plugins registered before the seed pass
// don't get silently disabled (defensive default; the boot path always
// seeds before StartAll, so the safety net is for edge cases).
func (r *PluginStateRepository) IsEnabled(pluginID string) (bool, error) {
	var enabled int
	err := r.db.QueryRow(
		`SELECT enabled FROM plugin_state WHERE plugin_id = ?`,
		pluginID,
	).Scan(&enabled)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return enabled != 0, nil
}

// Upsert is the install/update path used by lifecycle-07 (install API).
// Bumps version + signature + last_checked_at; preserves enabled so a
// re-install of an already-disabled plugin stays disabled.
func (r *PluginStateRepository) Upsert(s *domain.PluginState) error {
	if s.PluginID == "" {
		return fmt.Errorf("plugin_id required")
	}
	if !s.Distribution.IsValid() {
		return fmt.Errorf("invalid distribution %q", s.Distribution)
	}
	if s.InstalledAt.IsZero() {
		s.InstalledAt = time.Now().UTC()
	}
	_, err := r.db.Exec(
		`INSERT INTO plugin_state (plugin_id, distribution, installed, enabled, version,
		                           available_version, source_url, signature_fingerprint,
		                           installed_at, last_checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(plugin_id) DO UPDATE SET
		   distribution          = excluded.distribution,
		   installed             = excluded.installed,
		   version               = excluded.version,
		   available_version     = excluded.available_version,
		   source_url            = excluded.source_url,
		   signature_fingerprint = excluded.signature_fingerprint,
		   last_checked_at       = excluded.last_checked_at`,
		s.PluginID, string(s.Distribution), boolToInt(s.Installed), boolToInt(s.Enabled),
		s.Version, s.AvailableVersion, s.SourceURL, s.SignatureFingerprint,
		s.InstalledAt, s.LastCheckedAt,
	)
	return err
}

// Delete removes the row entirely. Used by lifecycle-07's uninstall
// path. System plugins should not be deleted; the caller (REST handler)
// is responsible for refusing system-plugin uninstall before reaching
// this method.
func (r *PluginStateRepository) Delete(pluginID string) error {
	_, err := r.db.Exec(`DELETE FROM plugin_state WHERE plugin_id = ?`, pluginID)
	return err
}

func scanPluginState(row rowScanner) (*domain.PluginState, error) {
	var (
		s           domain.PluginState
		installed   int
		enabled     int
		distStr     string
		lastChecked sql.NullTime
	)
	err := row.Scan(
		&s.PluginID, &distStr, &installed, &enabled, &s.Version,
		&s.AvailableVersion, &s.SourceURL, &s.SignatureFingerprint,
		&s.InstalledAt, &lastChecked,
	)
	if err != nil {
		return nil, err
	}
	s.Distribution = domain.PluginDistribution(distStr)
	s.Installed = installed != 0
	s.Enabled = enabled != 0
	if lastChecked.Valid {
		t := lastChecked.Time
		s.LastCheckedAt = &t
	}
	return &s, nil
}
