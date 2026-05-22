package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/felixgeelhaar/mnemos"
	"github.com/felixgeelhaar/mnemos/embedded"

	"github.com/felixgeelhaar/nomi/internal/storage/db"
)

// migrationCompletedKey is the app_settings key that records a
// completed legacy-memory migration. Presence (regardless of value)
// means "do not re-run." Value is the ISO-8601 completion timestamp.
const migrationCompletedKey = "mnemos_legacy_migration_completed_at"

// legacyMigrationBatchLimit caps how many rows we read in one Retrieve
// during the migration. The corpora we expect are small (single
// digits of thousands at most) so a single batch suffices; the limit
// is a defense-in-depth against an unexpectedly large legacy table.
const legacyMigrationBatchLimit = 10_000

// MigrateLegacyMemory is a one-shot importer that copies rows from
// the nomi.db legacy `memory` table into the new mnemos.db file. It
// records completion in app_settings so subsequent boots short-circuit
// without doing the read.
//
// The legacy table is left intact — rolling back to a pre-ADR-0004
// build remains possible until a follow-up cleanup migration drops the
// column. Callers should treat the migration as best-effort: failure
// logs a warning rather than aborting startup, on the theory that an
// empty new store is recoverable while a refusal to boot is not.
func MigrateLegacyMemory(ctx context.Context, database *db.DB, dst *embedded.Client, src *Manager) error {
	if database == nil || dst == nil || src == nil {
		return fmt.Errorf("MigrateLegacyMemory: nil dependency")
	}

	settings := db.NewAppSettingsRepository(database)
	if existing, _ := settings.Get(migrationCompletedKey); existing != "" {
		return nil // already ran
	}

	// Read every row from the legacy table. Manager.ListByScope("")
	// is not supported (scope is required), so we union the two
	// known legacy scopes that the runtime ever wrote.
	scopes := []string{"workspace", "profile", "preferences"}
	var total int
	for _, legacyScope := range scopes {
		rows, err := src.ListByScope(legacyScope, legacyMigrationBatchLimit)
		if err != nil {
			return fmt.Errorf("read legacy scope %q: %w", legacyScope, err)
		}
		if len(rows) == 0 {
			continue
		}
		destScope := scopeFromLegacy(legacyScope)
		for _, r := range rows {
			entry := &mnemos.Entry{
				ID:          r.ID,
				Content:     r.Content,
				AssistantID: r.AssistantID,
				RunID:       r.RunID,
				CreatedAt:   r.CreatedAt,
			}
			if err := dst.Store(ctx, destScope, entry); err != nil {
				// Skip duplicates (PK violation on re-import) but
				// surface unexpected errors so the operator knows.
				slog.Warn("legacy memory import: skip row", "id", r.ID, "scope", legacyScope, "error", err)
				continue
			}
			total++
		}
	}

	if err := settings.Set(migrationCompletedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}
	slog.Info("legacy memory migration", "rows_imported", total)
	return nil
}

// scopeFromLegacy converts the flat legacy scope string into the
// modern mnemos.Scope tuple. Mirrors the runtime's scopeFromPolicy
// mapping so the two paths cannot drift.
func scopeFromLegacy(legacy string) mnemos.Scope {
	switch legacy {
	case "profile":
		return mnemos.LocalProfile()
	case "preferences":
		return mnemos.LocalPreferences()
	case "workspace", "":
		return mnemos.LocalWorkspace()
	default:
		return mnemos.LocalWorkspace()
	}
}
