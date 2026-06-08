package seed

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// inMemSecrets is a tiny secrets.Store impl for tests; production
// uses keyring or encrypted file. Not exported because the production
// code paths never need this shape.
type inMemSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func newInMemSecrets() secrets.Store {
	return &inMemSecrets{m: map[string]string{}}
}
func (s *inMemSecrets) Put(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}
func (s *inMemSecrets) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return "", secrets.ErrNotFound
}
func (s *inMemSecrets) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, k)
	return nil
}

func newDB(t *testing.T) Deps {
	t.Helper()
	dir := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	store := newInMemSecrets()
	return Deps{
		DB:         database,
		Providers:  db.NewProviderProfileRepository(database),
		Assistants: db.NewAssistantRepository(database),
		Settings:   db.NewAppSettingsRepository(database),
		Globals:    db.NewGlobalSettingsRepository(database),
		Secrets:    store,
	}
}

func writeSeed(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "seed.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyMissingFileIsNoop(t *testing.T) {
	deps := newDB(t)
	if err := Apply("/nonexistent/path.yaml", deps); err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
}

func TestApplyFullSeed(t *testing.T) {
	deps := newDB(t)
	dir := t.TempDir()
	path := writeSeed(t, dir, `
provider:
  name: Ollama
  type: local
  endpoint: http://localhost:11434
  model_ids: [qwen2.5:14b, llama3.2:latest]
  default_model: qwen2.5:14b
assistants:
  - template_id: research-assistant
    name: My Researcher
    workspace: /tmp/ws
settings:
  safety_profile: balanced
  onboarding_complete: true
`)
	if err := Apply(path, deps); err != nil {
		t.Fatalf("apply: %v", err)
	}
	provs, _ := deps.Providers.List()
	if len(provs) != 1 || provs[0].Name != "Ollama" {
		t.Fatalf("expected Ollama provider, got %+v", provs)
	}
	if provs[0].Endpoint != "http://localhost:11434/v1" {
		t.Fatalf("expected /v1 normalisation, got %q", provs[0].Endpoint)
	}
	provID, modelID := deps.Globals.GetLLMDefault()
	if provID != provs[0].ID || modelID != "qwen2.5:14b" {
		t.Fatalf("default not set: provider=%q model=%q", provID, modelID)
	}
	asses, _ := deps.Assistants.List(10, 0)
	if len(asses) != 1 || asses[0].Name != "My Researcher" {
		t.Fatalf("expected My Researcher, got %+v", asses)
	}
	if len(asses[0].Contexts) == 0 || asses[0].Contexts[0].Path != "/tmp/ws" {
		t.Fatalf("workspace context not applied: %+v", asses[0].Contexts)
	}
	if got := deps.Settings.GetOrDefault("safety_profile", ""); got != "balanced" {
		t.Fatalf("safety_profile = %q", got)
	}
	if got := deps.Settings.GetOrDefault("onboarding.complete", ""); got != "true" {
		t.Fatalf("onboarding.complete = %q", got)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	deps := newDB(t)
	dir := t.TempDir()
	path := writeSeed(t, dir, `
provider:
  name: Ollama
  type: local
  endpoint: http://localhost:11434
  model_ids: [qwen2.5:14b]
assistants:
  - template_id: research-assistant
    name: My Researcher
`)
	if err := Apply(path, deps); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(path, deps); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	provs, _ := deps.Providers.List()
	if len(provs) != 1 {
		t.Fatalf("expected 1 provider after rerun, got %d", len(provs))
	}
	asses, _ := deps.Assistants.List(10, 0)
	if len(asses) != 1 {
		t.Fatalf("expected 1 assistant after rerun, got %d", len(asses))
	}
}

func TestApplyRejectsBadEndpoint(t *testing.T) {
	deps := newDB(t)
	dir := t.TempDir()
	path := writeSeed(t, dir, `
provider:
  name: Bad
  type: local
  endpoint: file:///etc/passwd
  model_ids: [x]
`)
	if err := Apply(path, deps); err == nil {
		t.Fatal("expected error for file:// endpoint")
	}
}

func TestApplyRejectsBadSafetyProfile(t *testing.T) {
	deps := newDB(t)
	dir := t.TempDir()
	path := writeSeed(t, dir, `
settings:
  safety_profile: yolo
`)
	if err := Apply(path, deps); err == nil {
		t.Fatal("expected error for unknown safety_profile")
	}
}
