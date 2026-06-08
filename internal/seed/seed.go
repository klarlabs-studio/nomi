// Package seed bootstraps a fresh nomid install from a YAML manifest.
// Designed for headless / docker / kubernetes deploys where there's no
// onboarding wizard to walk through. Idempotent: rerunning with the
// same manifest is a no-op; editing the manifest and restarting picks
// up the diff (provider/assistant entries are matched by name).
//
// File schema:
//
//	provider:
//	  name: Ollama
//	  type: local                    # local | remote
//	  endpoint: http://ollama:11434
//	  model_ids: [qwen2.5:14b]
//	  default_model: qwen2.5:14b     # optional; first model_id is used otherwise
//	  api_key: ""                    # optional; remote providers
//	assistants:
//	  - template_id: research-assistant
//	    name: My Researcher
//	    workspace: /data/workspace/research
//	settings:
//	  safety_profile: balanced       # cautious | balanced | fast
//	  onboarding_complete: true
package seed

import (
	"encoding/json"
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
	assistanttemplates "go.klarlabs.de/nomi/templates"
)

// File is the YAML shape on disk.
type File struct {
	Provider   *ProviderSeed   `yaml:"provider,omitempty"`
	Assistants []AssistantSeed `yaml:"assistants,omitempty"`
	Settings   *SettingsSeed   `yaml:"settings,omitempty"`
}

type ProviderSeed struct {
	Name         string   `yaml:"name"`
	Type         string   `yaml:"type"`
	Endpoint     string   `yaml:"endpoint"`
	ModelIDs     []string `yaml:"model_ids"`
	DefaultModel string   `yaml:"default_model,omitempty"`
	APIKey       string   `yaml:"api_key,omitempty"`
}

type AssistantSeed struct {
	TemplateID string `yaml:"template_id"`
	Name       string `yaml:"name,omitempty"`
	Workspace  string `yaml:"workspace,omitempty"`
}

type SettingsSeed struct {
	SafetyProfile      string `yaml:"safety_profile,omitempty"`
	OnboardingComplete *bool  `yaml:"onboarding_complete,omitempty"`
}

// Deps bundles the repositories Apply needs. Passed in rather than
// constructed inside the package so cmd/nomid/main.go can wire its
// already-instantiated singletons.
type Deps struct {
	DB         *db.DB
	Providers  *db.ProviderProfileRepository
	Assistants *db.AssistantRepository
	Settings   *db.AppSettingsRepository
	Globals    *db.GlobalSettingsRepository
	Secrets    secrets.Store
}

// Apply parses the YAML at path and applies it idempotently. Returns
// nil if the file doesn't exist (no seed configured). Bigger errors
// (parse failures, repository errors) are returned to the caller; the
// daemon is expected to log the error but continue booting so a
// malformed seed doesn't take down a previously-working install.
func Apply(path string, deps Deps) error {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: caller-supplied seed file path
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("seed: read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("seed: parse %s: %w", path, err)
	}

	var providerID, defaultModel string
	if f.Provider != nil {
		providerID, defaultModel, err = applyProvider(deps, *f.Provider)
		if err != nil {
			return fmt.Errorf("seed provider: %w", err)
		}
		if providerID != "" {
			if err := deps.Globals.SetLLMDefault(providerID, defaultModel); err != nil {
				return fmt.Errorf("seed default: %w", err)
			}
			log.Printf("seed: default LLM set to %s/%s", providerID, defaultModel)
		}
	}

	for _, a := range f.Assistants {
		if err := applyAssistant(deps, a); err != nil {
			return fmt.Errorf("seed assistant %q: %w", a.Name, err)
		}
	}

	if f.Settings != nil {
		if f.Settings.SafetyProfile != "" {
			if !permissions.IsValidSafetyProfile(f.Settings.SafetyProfile) {
				return fmt.Errorf("seed: invalid safety_profile %q", f.Settings.SafetyProfile)
			}
			if err := deps.Settings.Set("safety_profile", f.Settings.SafetyProfile); err != nil {
				return fmt.Errorf("seed safety_profile: %w", err)
			}
		}
		if f.Settings.OnboardingComplete != nil {
			val := "false"
			if *f.Settings.OnboardingComplete {
				val = "true"
			}
			if err := deps.Settings.Set("onboarding.complete", val); err != nil {
				return fmt.Errorf("seed onboarding.complete: %w", err)
			}
		}
	}
	log.Printf("seed: applied %s", path)
	return nil
}

// applyProvider creates a provider profile by name (idempotent: if a
// profile with the same name already exists, returns its id without
// touching it). Returns the resolved (providerID, defaultModelID).
func applyProvider(deps Deps, p ProviderSeed) (string, string, error) {
	if p.Name == "" {
		return "", "", fmt.Errorf("provider.name is required")
	}
	if len(p.ModelIDs) == 0 {
		return "", "", fmt.Errorf("provider.model_ids must list at least one model")
	}
	endpoint, err := llm.NormalizeEndpoint(p.Endpoint)
	if err != nil {
		return "", "", fmt.Errorf("provider.endpoint: %w", err)
	}
	defaultModel := p.DefaultModel
	if defaultModel == "" {
		defaultModel = p.ModelIDs[0]
	}

	existing, err := deps.Providers.List()
	if err != nil {
		return "", "", err
	}
	for _, e := range existing {
		if e.Name == p.Name {
			log.Printf("seed: provider %q already present (id=%s); skipping", p.Name, e.ID)
			return e.ID, defaultModel, nil
		}
	}

	id := uuid.New().String()
	secretRef := ""
	if p.APIKey != "" && deps.Secrets != nil {
		ref, err := secrets.StoreAsReference(deps.Secrets,
			fmt.Sprintf("provider/%s/api_key", id), p.APIKey)
		if err != nil {
			return "", "", fmt.Errorf("stash api_key: %w", err)
		}
		secretRef = ref
	}

	if err := deps.Providers.Create(&domain.ProviderProfile{
		ID:        id,
		Name:      p.Name,
		Type:      p.Type,
		Endpoint:  endpoint,
		ModelIDs:  p.ModelIDs,
		SecretRef: secretRef,
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return "", "", err
	}
	log.Printf("seed: provider %q created (id=%s, endpoint=%s)", p.Name, id, endpoint)
	return id, defaultModel, nil
}

// applyAssistant materialises an assistant from a template. Idempotent
// by Name: existing assistants with the same name are left alone.
func applyAssistant(deps Deps, a AssistantSeed) error {
	if a.TemplateID == "" {
		return fmt.Errorf("assistant.template_id is required")
	}
	tpl, err := assistanttemplates.ByID(a.TemplateID)
	if err != nil {
		return fmt.Errorf("template %q: %w", a.TemplateID, err)
	}

	name := a.Name
	if name == "" {
		name = tpl.Name
	}

	existing, err := deps.Assistants.List(1000, 0)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.Name == name {
			log.Printf("seed: assistant %q already present (id=%s); skipping", name, e.ID)
			return nil
		}
	}

	contexts := tpl.Contexts
	if a.Workspace != "" {
		contexts = []domain.ContextAttachment{{Type: "folder", Path: a.Workspace}}
	}

	def := &domain.AssistantDefinition{
		ID:                  uuid.New().String(),
		TemplateID:          tpl.TemplateID,
		Name:                name,
		Tagline:             tpl.Tagline,
		Role:                tpl.Role,
		BestFor:             tpl.BestFor,
		NotFor:              tpl.NotFor,
		SuggestedModel:      tpl.SuggestedModel,
		SystemPrompt:        tpl.SystemPrompt,
		Channels:            tpl.Channels,
		ChannelConfigs:      tpl.ChannelConfigs,
		Capabilities:        tpl.Capabilities,
		Contexts:            contexts,
		MemoryPolicy:        tpl.MemoryPolicy,
		PermissionPolicy:    tpl.PermissionPolicy,
		ModelPolicy:         tpl.ModelPolicy,
		RecommendedBindings: tpl.RecommendedBindings,
		CreatedAt:           time.Now().UTC(),
	}
	if err := deps.Assistants.Create(def); err != nil {
		return err
	}
	// Marshal-roundtrip just to surface any JSON encoding issues now
	// rather than on the next list. Cheap; fail-fast.
	if _, err := json.Marshal(def); err != nil {
		return fmt.Errorf("marshal sanity check: %w", err)
	}
	log.Printf("seed: assistant %q created (id=%s, template=%s)", name, def.ID, tpl.TemplateID)
	return nil
}
