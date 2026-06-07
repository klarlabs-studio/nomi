// Package configio snapshots and restores user-configured daemon
// state. The output is a YAML document a user can commit to git, ship
// to a teammate, or apply on a fresh install to reproduce the same
// providers, assistants, settings, preferences, and plugin
// enablement.
//
// What's covered today:
//
//   - provider profiles (with model lists; SECRETS NOT EXPORTED)
//   - the global default LLM (provider name + model id)
//   - assistants (template, name, persona, capabilities, contexts,
//     permission policy, model policy)
//   - app-level settings (safety profile, onboarding flag)
//   - memory entries in the `preferences` scope (workspace + profile
//     are runtime-derived; restoring them across machines would
//     create misleading provenance)
//   - per-plugin enabled flags
//
// What's NOT covered:
//
//   - secrets / API keys — exports include only the secret reference
//     URI; on import, the user is expected to set them via the UI or
//     out-of-band before runs that need them work.
//   - runs / steps / events / approvals (history, not config).
//   - plugin connections — multi-step OAuth + bot-token flows that
//     don't survive a config-only export. Track in a follow-up.
//
// Files use stable provider/assistant NAMES rather than UUIDs so a
// snapshot from machine A can apply on machine B without colliding
// with B's already-existing IDs.
package configio

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/llm"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// SchemaVersion bumps any time the on-disk shape changes in a way the
// import path needs to handle. Stored at the top of every file so a
// future major schema rev doesn't silently corrupt older exports.
const SchemaVersion = 1

// Snapshot is the wire shape. Every field is omit-empty so a sparse
// import (e.g. only the providers section) round-trips cleanly.
type Snapshot struct {
	SchemaVersion int                 `yaml:"schema_version"`
	ExportedAt    time.Time           `yaml:"exported_at,omitempty"`
	Providers     []ProviderSnapshot  `yaml:"providers,omitempty"`
	DefaultLLM    *DefaultLLM         `yaml:"default_llm,omitempty"`
	Assistants    []AssistantSnapshot `yaml:"assistants,omitempty"`
	Settings      *SettingsSnapshot   `yaml:"settings,omitempty"`
	Preferences   []MemorySnapshot    `yaml:"preferences,omitempty"`
	PluginStates  []PluginState       `yaml:"plugin_states,omitempty"`
}

type ProviderSnapshot struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	Endpoint  string   `yaml:"endpoint"`
	ModelIDs  []string `yaml:"model_ids"`
	Enabled   bool     `yaml:"enabled"`
	SecretRef string   `yaml:"secret_ref,omitempty"` // reference only; never the plaintext
}

type DefaultLLM struct {
	ProviderName string `yaml:"provider_name"`
	ModelID      string `yaml:"model_id"`
}

type AssistantSnapshot struct {
	TemplateID       string                     `yaml:"template_id,omitempty"`
	Name             string                     `yaml:"name"`
	Tagline          string                     `yaml:"tagline,omitempty"`
	Role             string                     `yaml:"role,omitempty"`
	BestFor          string                     `yaml:"best_for,omitempty"`
	NotFor           string                     `yaml:"not_for,omitempty"`
	SuggestedModel   string                     `yaml:"suggested_model,omitempty"`
	SystemPrompt     string                     `yaml:"system_prompt,omitempty"`
	Channels         []string                   `yaml:"channels,omitempty"`
	Capabilities     []string                   `yaml:"capabilities,omitempty"`
	Contexts         []domain.ContextAttachment `yaml:"contexts,omitempty"`
	MemoryPolicy     domain.MemoryPolicy        `yaml:"memory_policy,omitempty"`
	PermissionPolicy domain.PermissionPolicy    `yaml:"permission_policy,omitempty"`
	ModelPolicy      *domain.ModelPolicy        `yaml:"model_policy,omitempty"`
}

type SettingsSnapshot struct {
	SafetyProfile      string `yaml:"safety_profile,omitempty"`
	OnboardingComplete *bool  `yaml:"onboarding_complete,omitempty"`
}

type MemorySnapshot struct {
	Scope   string `yaml:"scope"`
	Content string `yaml:"content"`
}

type PluginState struct {
	ID           string   `yaml:"id"`
	Enabled      bool     `yaml:"enabled"`
	EnabledRoles []string `yaml:"enabled_roles,omitempty"`
}

// Deps mirrors the seed package's bundle so cmd/nomid/main.go can pass
// already-instantiated repositories rather than re-opening them.
type Deps struct {
	DB           *db.DB
	Providers    *db.ProviderProfileRepository
	Assistants   *db.AssistantRepository
	Settings     *db.AppSettingsRepository
	Globals      *db.GlobalSettingsRepository
	Memory       *db.MemoryRepository
	PluginStates *db.PluginStateRepository
	Secrets      secrets.Store
}

// Export captures the current daemon state into a Snapshot. Idempotent
// on the daemon side (read-only).
func Export(deps Deps) (*Snapshot, error) {
	snap := &Snapshot{
		SchemaVersion: SchemaVersion,
		ExportedAt:    time.Now().UTC(),
	}

	provs, err := deps.Providers.List()
	if err != nil {
		return nil, fmt.Errorf("export providers: %w", err)
	}
	for _, p := range provs {
		snap.Providers = append(snap.Providers, ProviderSnapshot{
			Name:      p.Name,
			Type:      p.Type,
			Endpoint:  p.Endpoint,
			ModelIDs:  p.ModelIDs,
			Enabled:   p.Enabled,
			SecretRef: p.SecretRef, // reference URI only; plaintext stays in the secret store
		})
	}

	if pid, mid := deps.Globals.GetLLMDefault(); pid != "" {
		// Resolve provider id back to name for portability.
		for _, p := range provs {
			if p.ID == pid {
				snap.DefaultLLM = &DefaultLLM{ProviderName: p.Name, ModelID: mid}
				break
			}
		}
	}

	asses, err := deps.Assistants.List(1000, 0)
	if err != nil {
		return nil, fmt.Errorf("export assistants: %w", err)
	}
	for _, a := range asses {
		snap.Assistants = append(snap.Assistants, AssistantSnapshot{
			TemplateID:       a.TemplateID,
			Name:             a.Name,
			Tagline:          a.Tagline,
			Role:             a.Role,
			BestFor:          a.BestFor,
			NotFor:           a.NotFor,
			SuggestedModel:   a.SuggestedModel,
			SystemPrompt:     a.SystemPrompt,
			Channels:         a.Channels,
			Capabilities:     a.Capabilities,
			Contexts:         a.Contexts,
			MemoryPolicy:     a.MemoryPolicy,
			PermissionPolicy: a.PermissionPolicy,
			ModelPolicy:      a.ModelPolicy,
		})
	}

	settings := &SettingsSnapshot{}
	if v := deps.Settings.GetOrDefault("safety_profile", ""); v != "" {
		settings.SafetyProfile = v
	}
	if v := deps.Settings.GetOrDefault("onboarding.complete", ""); v != "" {
		complete := v == "true"
		settings.OnboardingComplete = &complete
	}
	if settings.SafetyProfile != "" || settings.OnboardingComplete != nil {
		snap.Settings = settings
	}

	prefs, err := deps.Memory.ListByScope("preferences", 1000)
	if err != nil {
		return nil, fmt.Errorf("export preferences: %w", err)
	}
	for _, m := range prefs {
		snap.Preferences = append(snap.Preferences, MemorySnapshot{
			Scope: m.Scope, Content: m.Content,
		})
	}

	if deps.PluginStates != nil {
		states, err := deps.PluginStates.List()
		if err == nil {
			for _, s := range states {
				snap.PluginStates = append(snap.PluginStates, PluginState{
					ID: s.PluginID, Enabled: s.Enabled,
				})
			}
		}
	}

	return snap, nil
}

// Import applies a Snapshot idempotently. Provider / assistant names
// match existing rows; if a row with the same name exists, it's
// updated in place. New rows are inserted. Settings are always
// overwritten when present in the snapshot.
//
// Returns a per-section count of created/updated/skipped so callers
// can render a summary on stdout.
func Import(snap *Snapshot, deps Deps) (Result, error) {
	if snap == nil {
		return Result{}, errors.New("nil snapshot")
	}
	if snap.SchemaVersion > SchemaVersion {
		return Result{}, fmt.Errorf("snapshot schema_version=%d is newer than this daemon (%d); upgrade nomid",
			snap.SchemaVersion, SchemaVersion)
	}

	var res Result
	providerIDByName := map[string]string{}

	// Providers first so the default LLM + assistant model_policy
	// references can resolve.
	existingProvs, err := deps.Providers.List()
	if err != nil {
		return res, fmt.Errorf("list existing providers: %w", err)
	}
	existingByName := map[string]*domain.ProviderProfile{}
	for _, p := range existingProvs {
		existingByName[p.Name] = p
	}
	for _, p := range snap.Providers {
		endpoint, err := llm.NormalizeEndpoint(p.Endpoint)
		if err != nil {
			return res, fmt.Errorf("provider %q endpoint: %w", p.Name, err)
		}
		if cur, ok := existingByName[p.Name]; ok {
			cur.Type = p.Type
			cur.Endpoint = endpoint
			cur.ModelIDs = p.ModelIDs
			cur.Enabled = p.Enabled
			// Don't overwrite secret_ref unless the snapshot specifies one
			// AND it's different — a fresh import shouldn't blow away a
			// secret the user set out-of-band.
			if p.SecretRef != "" {
				cur.SecretRef = p.SecretRef
			}
			cur.UpdatedAt = time.Now().UTC()
			if err := deps.Providers.Update(cur); err != nil {
				return res, fmt.Errorf("update provider %q: %w", p.Name, err)
			}
			providerIDByName[p.Name] = cur.ID
			res.ProvidersUpdated++
			continue
		}
		id := uuid.New().String()
		if err := deps.Providers.Create(&domain.ProviderProfile{
			ID:        id,
			Name:      p.Name,
			Type:      p.Type,
			Endpoint:  endpoint,
			ModelIDs:  p.ModelIDs,
			SecretRef: p.SecretRef,
			Enabled:   p.Enabled,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return res, fmt.Errorf("create provider %q: %w", p.Name, err)
		}
		providerIDByName[p.Name] = id
		res.ProvidersCreated++
	}

	if snap.DefaultLLM != nil {
		pid := providerIDByName[snap.DefaultLLM.ProviderName]
		if pid == "" {
			// Provider wasn't in this snapshot but might exist on the host.
			for _, p := range existingProvs {
				if p.Name == snap.DefaultLLM.ProviderName {
					pid = p.ID
					break
				}
			}
		}
		if pid == "" {
			log.Printf("import: default_llm references unknown provider %q; skipping",
				snap.DefaultLLM.ProviderName)
		} else if err := deps.Globals.SetLLMDefault(pid, snap.DefaultLLM.ModelID); err != nil {
			return res, fmt.Errorf("set default LLM: %w", err)
		}
	}

	existingAss, err := deps.Assistants.List(10000, 0)
	if err != nil {
		return res, fmt.Errorf("list assistants: %w", err)
	}
	existingAssByName := map[string]*domain.AssistantDefinition{}
	for _, a := range existingAss {
		existingAssByName[a.Name] = a
	}
	for _, a := range snap.Assistants {
		def := &domain.AssistantDefinition{
			TemplateID:       a.TemplateID,
			Name:             a.Name,
			Tagline:          a.Tagline,
			Role:             a.Role,
			BestFor:          a.BestFor,
			NotFor:           a.NotFor,
			SuggestedModel:   a.SuggestedModel,
			SystemPrompt:     a.SystemPrompt,
			Channels:         a.Channels,
			Capabilities:     a.Capabilities,
			Contexts:         a.Contexts,
			MemoryPolicy:     a.MemoryPolicy,
			PermissionPolicy: a.PermissionPolicy,
			ModelPolicy:      a.ModelPolicy,
		}
		if cur, ok := existingAssByName[a.Name]; ok {
			def.ID = cur.ID
			def.CreatedAt = cur.CreatedAt
			if err := deps.Assistants.Update(def); err != nil {
				return res, fmt.Errorf("update assistant %q: %w", a.Name, err)
			}
			res.AssistantsUpdated++
			continue
		}
		def.ID = uuid.New().String()
		def.CreatedAt = time.Now().UTC()
		if err := deps.Assistants.Create(def); err != nil {
			return res, fmt.Errorf("create assistant %q: %w", a.Name, err)
		}
		res.AssistantsCreated++
	}

	if snap.Settings != nil {
		if snap.Settings.SafetyProfile != "" {
			if !permissions.IsValidSafetyProfile(snap.Settings.SafetyProfile) {
				return res, fmt.Errorf("invalid safety_profile %q", snap.Settings.SafetyProfile)
			}
			if err := deps.Settings.Set("safety_profile", snap.Settings.SafetyProfile); err != nil {
				return res, err
			}
		}
		if snap.Settings.OnboardingComplete != nil {
			val := "false"
			if *snap.Settings.OnboardingComplete {
				val = "true"
			}
			if err := deps.Settings.Set("onboarding.complete", val); err != nil {
				return res, err
			}
		}
		res.SettingsApplied = true
	}

	// Preferences are matched by exact content — re-importing the same
	// snapshot doesn't double-up entries. Other scopes (workspace,
	// profile) are deliberately not imported; they're runtime-derived
	// from runs and reseeding them would yield misleading provenance.
	if len(snap.Preferences) > 0 {
		existing, err := deps.Memory.ListByScope("preferences", 10000)
		if err != nil {
			return res, fmt.Errorf("list preferences: %w", err)
		}
		seen := map[string]bool{}
		for _, m := range existing {
			seen[m.Content] = true
		}
		for _, m := range snap.Preferences {
			if seen[m.Content] {
				res.PreferencesSkipped++
				continue
			}
			if err := deps.Memory.Create(&domain.MemoryEntry{
				ID:        uuid.New().String(),
				Scope:     m.Scope,
				Content:   m.Content,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return res, fmt.Errorf("create preference: %w", err)
			}
			res.PreferencesCreated++
		}
	}

	if deps.PluginStates != nil {
		for _, s := range snap.PluginStates {
			if err := deps.PluginStates.SetEnabled(s.ID, s.Enabled); err != nil {
				log.Printf("import: plugin %q SetEnabled: %v", s.ID, err)
				continue
			}
			res.PluginStatesApplied++
		}
	}

	return res, nil
}

// Result is the per-section summary returned by Import. Surface to the
// user so they know what changed.
type Result struct {
	ProvidersCreated    int
	ProvidersUpdated    int
	AssistantsCreated   int
	AssistantsUpdated   int
	PreferencesCreated  int
	PreferencesSkipped  int
	PluginStatesApplied int
	SettingsApplied     bool
}

// MarshalYAML / Marshal helpers expose the encoder as a one-line API
// for callers that don't want to import yaml.v3 directly.
func Marshal(snap *Snapshot) ([]byte, error)     { return yaml.Marshal(snap) }
func Unmarshal(raw []byte, snap *Snapshot) error { return yaml.Unmarshal(raw, snap) }

// LoadFile is a convenience for `nomid` / `nomi` to read a snapshot
// straight off disk.
func LoadFile(path string) (*Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &snap, nil
}
