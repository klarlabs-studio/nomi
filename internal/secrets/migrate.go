package secrets

import (
	"encoding/json"
	"fmt"
	"log"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// MigrateRepositories moves any plaintext secrets still present in SQLite
// into the given Store and replaces the DB values with secret:// references.
// Safe to call on every boot: values already stored as references pass
// through unchanged.
//
// Covered paths:
//   - connector_configs.config (Telegram connections[].bot_token)
//   - provider_profiles.secret_ref
//
// Any row that cannot be migrated (malformed JSON, keyring error) is logged
// and left alone so one bad row doesn't block boot.
func MigrateRepositories(store Store, database *db.DB) error {
	if store == nil {
		return fmt.Errorf("secrets store is required")
	}
	if err := migrateConnectorConfigs(store, database); err != nil {
		log.Printf("secrets: connector migration: %v", err)
	}
	if err := migrateProviderProfiles(store, database); err != nil {
		log.Printf("secrets: provider migration: %v", err)
	}
	return nil
}

// telegramConfigLike mirrors TelegramConfig's JSON shape without pulling in
// the connectors package (which would create an import cycle, since the
// connectors package imports secrets).
type telegramConfigLike struct {
	Enabled     bool                     `json:"enabled"`
	Connections []telegramConnectionLike `json:"connections"`
}

type telegramConnectionLike struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	BotToken           string `json:"bot_token"`
	DefaultAssistantID string `json:"default_assistant_id,omitempty"`
	Enabled            bool   `json:"enabled"`
}

func migrateConnectorConfigs(store Store, database *db.DB) error {
	repo := db.NewConnectorConfigRepository(database)

	// Only the Telegram connector currently stores secrets; generalize when
	// a second secret-bearing connector arrives.
	rec, err := repo.Get("telegram")
	if err != nil {
		return fmt.Errorf("read telegram config: %w", err)
	}
	if rec == nil || len(rec.Config) == 0 {
		return nil
	}
	var cfg telegramConfigLike
	if err := json.Unmarshal(rec.Config, &cfg); err != nil {
		return fmt.Errorf("telegram config is not valid JSON: %w", err)
	}

	migrated := false
	for i := range cfg.Connections {
		conn := &cfg.Connections[i]
		if conn.BotToken == "" || IsReference(conn.BotToken) {
			continue
		}
		key := fmt.Sprintf("connector/telegram/%s/bot_token", conn.ID)
		ref, err := StoreAsReference(store, key, conn.BotToken)
		if err != nil {
			log.Printf("secrets: failed to migrate telegram[%s]: %v", conn.ID, err)
			continue
		}
		conn.BotToken = ref
		migrated = true
		log.Printf("secrets: migrated telegram[%s] bot_token → %s", conn.ID, ref)
	}

	if migrated {
		newBytes, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("re-marshal telegram config: %w", err)
		}
		if err := repo.Upsert("telegram", newBytes, rec.Enabled); err != nil {
			return fmt.Errorf("persist migrated telegram config: %w", err)
		}
	}
	return nil
}

func migrateProviderProfiles(store Store, database *db.DB) error {
	repo := db.NewProviderProfileRepository(database)
	profiles, err := repo.List()
	if err != nil {
		return fmt.Errorf("list provider profiles: %w", err)
	}
	for _, p := range profiles {
		if p.SecretRef == "" || IsReference(p.SecretRef) {
			continue
		}
		key := fmt.Sprintf("provider/%s/api_key", p.ID)
		ref, err := StoreAsReference(store, key, p.SecretRef)
		if err != nil {
			log.Printf("secrets: failed to migrate provider %s: %v", p.ID, err)
			continue
		}
		copy := *p
		copy.SecretRef = ref
		if err := repo.Update(&copy); err != nil {
			log.Printf("secrets: failed to persist migrated provider %s: %v", p.ID, err)
			continue
		}
		log.Printf("secrets: migrated provider %s api_key → %s", p.ID, ref)
	}
	return nil
}

// Compile-time check that we reference the domain import (used indirectly
// via repo types). Keeps the import tidy after future refactors.
var _ = domain.PermissionAllow
