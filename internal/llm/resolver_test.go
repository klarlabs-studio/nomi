package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/secrets"
)

type memoryProfiles struct {
	mu       sync.Mutex
	profiles map[string]*domain.ProviderProfile
}

func (m *memoryProfiles) GetByID(id string) (*domain.ProviderProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.profiles[id], nil
}

type memorySettings struct{ providerID, modelID string }

func (m *memorySettings) GetLLMDefault() (string, string) { return m.providerID, m.modelID }

type memoryStore struct {
	mu   sync.Mutex
	data map[string]string
}

func (s *memoryStore) Put(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.data[k] = v
	return nil
}

func (s *memoryStore) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

func (s *memoryStore) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, k)
	return nil
}

func TestResolverDefaultClientReturnsNilWhenNoProvider(t *testing.T) {
	res := NewResolver(&memoryProfiles{}, &memorySettings{}, &memoryStore{})
	client, model, err := res.DefaultClient()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client != nil || model != "" {
		t.Fatalf("expected (nil, \"\", nil) when unconfigured; got (%v, %q)", client, model)
	}
}

func TestResolverBuildsClientFromProfile(t *testing.T) {
	// Stand up a fake OpenAI-compat endpoint that records the bearer token
	// so we can confirm the secret was resolved through the store and not
	// passed plaintext from the ProviderProfile.
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openaiChatResponse{
			Choices: []struct {
				Message ChatMessage `json:"message"`
			}{{Message: ChatMessage{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer srv.Close()

	store := &memoryStore{}
	_ = store.Put("provider/p1/api_key", "sk-real")

	profiles := &memoryProfiles{profiles: map[string]*domain.ProviderProfile{
		"p1": {
			ID:        "p1",
			Name:      "test",
			Type:      "remote",
			Endpoint:  srv.URL,
			ModelIDs:  []string{"gpt-foo"},
			SecretRef: "secret://provider/p1/api_key",
			Enabled:   true,
		},
	}}
	settings := &memorySettings{providerID: "p1", modelID: "gpt-foo"}

	res := NewResolver(profiles, settings, store)
	client, model, err := res.DefaultClient()
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("expected a client")
	}
	if model != "gpt-foo" {
		t.Fatalf("model: %q", model)
	}

	// Actually invoke so we can assert the secret flowed through.
	if _, err := client.Chat(context.Background(), ChatRequest{
		Model:    "gpt-foo",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer sk-real" {
		t.Fatalf("secret not resolved: %q", seenAuth)
	}
}

func TestResolverRejectsDisabledProvider(t *testing.T) {
	profiles := &memoryProfiles{profiles: map[string]*domain.ProviderProfile{
		"p1": {ID: "p1", Enabled: false, Endpoint: "http://x"},
	}}
	res := NewResolver(profiles, &memorySettings{providerID: "p1"}, &memoryStore{})
	if _, _, err := res.DefaultClient(); err == nil {
		t.Fatal("expected error for disabled provider")
	}
}

func TestResolverCachesClients(t *testing.T) {
	profiles := &memoryProfiles{profiles: map[string]*domain.ProviderProfile{
		"p1": {ID: "p1", Enabled: true, Endpoint: "http://example.com", ModelIDs: []string{"m"}},
	}}
	res := NewResolver(profiles, &memorySettings{providerID: "p1"}, &memoryStore{})
	c1, err := res.ClientForProfile("p1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := res.ClientForProfile("p1")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected cached client identity on second lookup")
	}

	// After invalidation, a new client is built.
	res.InvalidateCache("p1")
	c3, err := res.ClientForProfile("p1")
	if err != nil {
		t.Fatal(err)
	}
	if c3 == c1 {
		t.Fatal("expected new client after invalidation")
	}
}
