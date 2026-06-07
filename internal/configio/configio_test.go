package configio

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// inMemSecrets is the same minimal Store fake the seed package uses
// in its tests. Production uses keyring or encrypted file.
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

func newDeps(t *testing.T) Deps {
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
	return Deps{
		DB:           database,
		Providers:    db.NewProviderProfileRepository(database),
		Assistants:   db.NewAssistantRepository(database),
		Settings:     db.NewAppSettingsRepository(database),
		Globals:      db.NewGlobalSettingsRepository(database),
		Memory:       db.NewMemoryRepository(database),
		PluginStates: db.NewPluginStateRepository(database),
		Secrets:      newInMemSecrets(),
	}
}

// seedDaemon plants the surface Export should round-trip: one
// provider, one assistant, both settings, one preference memory.
func seedDaemon(t *testing.T, d Deps) {
	t.Helper()
	prov := &domain.ProviderProfile{
		ID: uuid.New().String(), Name: "Ollama (Local)", Type: "local",
		Endpoint: "http://127.0.0.1:11434/v1",
		ModelIDs: []string{"qwen2.5:14b"}, Enabled: true,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := d.Providers.Create(prov); err != nil {
		t.Fatal(err)
	}
	if err := d.Globals.SetLLMDefault(prov.ID, "qwen2.5:14b"); err != nil {
		t.Fatal(err)
	}
	if err := d.Assistants.Create(&domain.AssistantDefinition{
		ID: uuid.New().String(), Name: "Researcher", Role: "research",
		SystemPrompt: "You are a research assistant.",
		PermissionPolicy: domain.PermissionPolicy{Rules: []domain.PermissionRule{
			{Capability: "llm.chat", Mode: domain.PermissionAllow},
		}},
		Capabilities: []string{"llm.chat"},
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.Settings.Set("safety_profile", "balanced"); err != nil {
		t.Fatal(err)
	}
	if err := d.Settings.Set("onboarding.complete", "true"); err != nil {
		t.Fatal(err)
	}
	if err := d.Memory.Create(&domain.MemoryEntry{
		ID: uuid.New().String(), Scope: "preferences",
		Content:   "Always cite sources.",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExportCapturesSeededState(t *testing.T) {
	d := newDeps(t)
	seedDaemon(t, d)

	snap, err := Export(d)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", snap.SchemaVersion, SchemaVersion)
	}
	if len(snap.Providers) != 1 || snap.Providers[0].Name != "Ollama (Local)" {
		t.Fatalf("providers: %+v", snap.Providers)
	}
	if snap.DefaultLLM == nil || snap.DefaultLLM.ProviderName != "Ollama (Local)" {
		t.Fatalf("default_llm: %+v", snap.DefaultLLM)
	}
	if len(snap.Assistants) != 1 || snap.Assistants[0].Name != "Researcher" {
		t.Fatalf("assistants: %+v", snap.Assistants)
	}
	if snap.Settings == nil || snap.Settings.SafetyProfile != "balanced" {
		t.Fatalf("settings: %+v", snap.Settings)
	}
	if snap.Settings.OnboardingComplete == nil || !*snap.Settings.OnboardingComplete {
		t.Fatalf("onboarding_complete: %+v", snap.Settings.OnboardingComplete)
	}
	if len(snap.Preferences) != 1 || snap.Preferences[0].Content != "Always cite sources." {
		t.Fatalf("preferences: %+v", snap.Preferences)
	}
}

// Round-trip: export from one daemon, import into a fresh one, expect
// the second daemon to have the same surface.
func TestImportFromExportReproduces(t *testing.T) {
	src := newDeps(t)
	seedDaemon(t, src)
	snap, err := Export(src)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	dst := newDeps(t)
	var dstSnap Snapshot
	if err := Unmarshal(raw, &dstSnap); err != nil {
		t.Fatal(err)
	}
	res, err := Import(&dstSnap, dst)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.ProvidersCreated != 1 || res.AssistantsCreated != 1 {
		t.Fatalf("create counts: %+v", res)
	}
	if !res.SettingsApplied || res.PreferencesCreated != 1 {
		t.Fatalf("settings/prefs: %+v", res)
	}

	// Verify dst now matches what we exported.
	provs, _ := dst.Providers.List()
	if len(provs) != 1 || provs[0].Name != "Ollama (Local)" {
		t.Fatalf("dst providers: %+v", provs)
	}
	pid, mid := dst.Globals.GetLLMDefault()
	if pid != provs[0].ID || mid != "qwen2.5:14b" {
		t.Fatalf("dst default: %s/%s want %s/qwen2.5:14b", pid, mid, provs[0].ID)
	}
	asses, _ := dst.Assistants.List(10, 0)
	if len(asses) != 1 || asses[0].Name != "Researcher" {
		t.Fatalf("dst assistants: %+v", asses)
	}
	prefs, _ := dst.Memory.ListByScope("preferences", 10)
	if len(prefs) != 1 {
		t.Fatalf("dst preferences: %+v", prefs)
	}
}

// Re-importing the same snapshot must not duplicate rows.
func TestImportIsIdempotent(t *testing.T) {
	d := newDeps(t)
	seedDaemon(t, d)
	snap, _ := Export(d)

	first, err := Import(snap, d)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Import(snap, d)
	if err != nil {
		t.Fatal(err)
	}
	// First call updates existing rows; second call should still
	// resolve to updates (never new creates).
	if first.ProvidersCreated != 0 || second.ProvidersCreated != 0 {
		t.Fatalf("expected no creates on rerun, got first=%+v second=%+v", first, second)
	}
	if second.PreferencesCreated != 0 {
		t.Fatalf("preferences re-inserted (content dedupe broken): %+v", second)
	}
	provs, _ := d.Providers.List()
	if len(provs) != 1 {
		t.Fatalf("provider count drifted: %d", len(provs))
	}
	prefs, _ := d.Memory.ListByScope("preferences", 10)
	if len(prefs) != 1 {
		t.Fatalf("preferences duplicated: %d", len(prefs))
	}
}

// A future schema bump should refuse to load on an older daemon.
func TestImportRejectsNewerSchema(t *testing.T) {
	d := newDeps(t)
	snap := &Snapshot{SchemaVersion: SchemaVersion + 1}
	if _, err := Import(snap, d); err == nil {
		t.Fatal("expected schema_version mismatch error")
	}
}

// Default LLM by name resolves against an existing provider on the
// destination host even if that provider wasn't in the snapshot.
func TestImportDefaultLLMResolvesAgainstExistingProvider(t *testing.T) {
	d := newDeps(t)
	// Pre-existing provider on the destination — the snapshot only
	// carries the default_llm reference, no provider section.
	prov := &domain.ProviderProfile{
		ID: "abc-existing", Name: "Ollama", Type: "local",
		Endpoint: "http://127.0.0.1:11434/v1", ModelIDs: []string{"x"},
		Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := d.Providers.Create(prov); err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{
		SchemaVersion: SchemaVersion,
		DefaultLLM:    &DefaultLLM{ProviderName: "Ollama", ModelID: "x"},
	}
	if _, err := Import(snap, d); err != nil {
		t.Fatal(err)
	}
	gotPID, gotMID := d.Globals.GetLLMDefault()
	if gotPID != "abc-existing" || gotMID != "x" {
		t.Fatalf("default not resolved: %s/%s", gotPID, gotMID)
	}
}

func TestImportRejectsBadSafetyProfile(t *testing.T) {
	d := newDeps(t)
	snap := &Snapshot{
		SchemaVersion: SchemaVersion,
		Settings:      &SettingsSnapshot{SafetyProfile: "yolo"},
	}
	if _, err := Import(snap, d); err == nil {
		t.Fatal("expected invalid safety_profile error")
	}
}
