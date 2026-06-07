package llm

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/secrets"
)

// ProfileRepo is the subset of ProviderProfileRepository the resolver needs.
// Keeping it as an interface lets tests use an in-memory implementation.
type ProfileRepo interface {
	GetByID(id string) (*domain.ProviderProfile, error)
}

// SettingsRepo is the subset of GlobalSettingsRepository the resolver needs.
type SettingsRepo interface {
	GetLLMDefault() (providerID, modelID string)
}

// Resolver turns ProviderProfile rows into ready-to-use Clients. It
// caches constructed clients per profile ID since HTTP clients (with their
// connection pools) are cheap to reuse and Go's net/http is safe for
// concurrent use.
type Resolver struct {
	profiles ProfileRepo
	settings SettingsRepo
	secrets  secrets.Store

	mu    sync.Mutex
	cache map[string]cachedClient
}

type cachedClient struct {
	client    Client
	modelHint string // the model ID associated with this profile for default calls
}

// NewResolver constructs a Resolver. Any of the dependencies may be nil
// only for tests that exercise a single path.
func NewResolver(profiles ProfileRepo, settings SettingsRepo, secretStore secrets.Store) *Resolver {
	return &Resolver{
		profiles: profiles,
		settings: settings,
		secrets:  secretStore,
		cache:    make(map[string]cachedClient),
	}
}

// DefaultClient returns the Client for the current global-default provider,
// plus the model ID the caller should use. If no default is configured the
// resolver returns (nil, "", nil) — that's the "no LLM wired" signal the
// runtime falls back from to legacy command.exec behavior. An error is only
// returned when a default IS configured but can't be constructed.
func (r *Resolver) DefaultClient() (Client, string, error) {
	if r == nil || r.settings == nil {
		return nil, "", nil
	}
	providerID, modelID := r.settings.GetLLMDefault()
	if providerID == "" {
		return nil, "", nil
	}
	client, err := r.ClientForProfile(providerID)
	if err != nil {
		return nil, "", err
	}
	if modelID == "" {
		// Fall back to the profile's first declared model so a provider
		// without an explicit default model is still usable.
		modelID = r.cache[providerID].modelHint
	}
	return client, modelID, nil
}

// ClientForProfile builds (or returns a cached) Client for the given
// profile ID.
func (r *Resolver) ClientForProfile(id string) (Client, error) {
	r.mu.Lock()
	if entry, ok := r.cache[id]; ok {
		r.mu.Unlock()
		return entry.client, nil
	}
	r.mu.Unlock()

	profile, err := r.profiles.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("llm: load provider %s: %w", id, err)
	}
	if !profile.Enabled {
		return nil, fmt.Errorf("llm: provider %s is disabled", id)
	}

	// Resolve the secret reference through the store; never log the plaintext.
	var apiKey string
	if profile.SecretRef != "" {
		if r.secrets == nil {
			return nil, fmt.Errorf("llm: provider %s has secret_ref but no secrets store is configured", id)
		}
		plain, err := secrets.Resolve(r.secrets, profile.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("llm: resolve secret for provider %s: %w", id, err)
		}
		apiKey = plain
	}

	endpointType := endpointTypeFor(profile.Endpoint)
	baseURL := profile.Endpoint
	if baseURL == "" {
		return nil, fmt.Errorf("llm: provider %s has no endpoint configured", id)
	}

	client, err := NewClient(Config{
		Type:    endpointType,
		BaseURL: baseURL,
		APIKey:  apiKey,
	})
	if err != nil {
		return nil, err
	}

	modelHint := ""
	if len(profile.ModelIDs) > 0 {
		modelHint = profile.ModelIDs[0]
	}

	r.mu.Lock()
	r.cache[id] = cachedClient{client: client, modelHint: modelHint}
	r.mu.Unlock()
	return client, nil
}

// DefaultEmbeddingClient returns an EmbeddingClient built from the
// default provider's endpoint + secret, with the embedding model
// pulled from the provider's `embedding_model_id` field (or the
// settings-level fallback). Returns nil, nil when no embeddings model
// is configured — caller's contract is "treat that as 'no embeddings
// available' and fall back to the heuristic path."
//
// Errors only surface when a default IS configured but can't be
// constructed (missing key, malformed endpoint).
func (r *Resolver) DefaultEmbeddingClient() (EmbeddingClient, error) {
	if r == nil || r.settings == nil {
		return nil, nil
	}
	providerID, _ := r.settings.GetLLMDefault()
	if providerID == "" {
		return nil, nil
	}
	profile, err := r.profiles.GetByID(providerID)
	if err != nil {
		return nil, fmt.Errorf("llm: load provider %s: %w", providerID, err)
	}
	if !profile.Enabled || profile.EmbeddingModelID == "" {
		return nil, nil
	}
	if endpointTypeFor(profile.Endpoint) != EndpointOpenAI {
		// Anthropic native API has no embeddings endpoint; callers fall
		// back to heuristic paths on that provider.
		return nil, nil
	}

	apiKey := ""
	if profile.SecretRef != "" {
		if r.secrets == nil {
			return nil, fmt.Errorf("llm: provider %s has secret_ref but no secrets store is configured", providerID)
		}
		plain, err := secrets.Resolve(r.secrets, profile.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("llm: resolve secret for provider %s: %w", providerID, err)
		}
		apiKey = plain
	}

	return NewEmbeddingClient(EmbeddingConfig{
		BaseURL:  profile.Endpoint,
		APIKey:   apiKey,
		Model:    profile.EmbeddingModelID,
		Provider: providerID,
	})
}

// InvalidateCacheIfAuthError checks if err is an AuthError (401)
// and clears the cached client for the profile so the next request
// obtains a fresh client with a new token.
func (r *Resolver) InvalidateCacheIfAuthError(id string, err error) bool {
	var authErr *AuthError
	if errors.As(err, &authErr) {
		r.InvalidateCache(id)
		return true
	}
	return false
}

// InvalidateCache clears the cached client for a profile. Called when the
// profile is updated (new endpoint, new key) so the next request picks up
// the change. Production code invokes this from the provider-update API
// handler; tests use it to swap clients mid-test.
func (r *Resolver) InvalidateCache(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, id)
}

// ModelHint returns the cached preferred model for a profile, if the client
// has been constructed. Returns "" if the profile hasn't been resolved yet
// or has no declared models.
func (r *Resolver) ModelHint(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cache[id].modelHint
}

// endpointTypeFor sniffs the endpoint URL to pick the right adapter.
// anthropic.com → Anthropic native; everything else → OpenAI-compat. Users
// can override by prefixing their endpoint with "anthropic+https://..." in
// a future enhancement; keeping the detection implicit for V1.
func endpointTypeFor(endpoint string) EndpointType {
	if strings.Contains(endpoint, "anthropic.com") {
		return EndpointAnthropic
	}
	return EndpointOpenAI
}
