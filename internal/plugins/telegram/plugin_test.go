package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// memSecrets is a minimal in-memory secrets.Store for plugin tests. We
// can't use the one in internal/api/smoke_test.go because that file's
// tests are in a different package.
type memSecrets struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemSecrets() *memSecrets { return &memSecrets{data: map[string]string{}} }
func (m *memSecrets) Put(k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = v
	return nil
}
func (m *memSecrets) Get(k string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[k]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (m *memSecrets) Delete(k string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, k)
	return nil
}

func newTestDB(t *testing.T) (*db.DB, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "nomi-tg-plugin-*.db")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	f.Close()
	database, err := db.New(db.Config{Path: f.Name()})
	if err != nil {
		os.Remove(f.Name())
		t.Fatalf("open db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		database.Close()
		os.Remove(f.Name())
		t.Fatalf("migrate: %v", err)
	}
	return database, func() {
		database.Close()
		os.Remove(f.Name())
	}
}

func TestManifestShape(t *testing.T) {
	p := &Plugin{apiBase: "http://unused"}
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("ID: %s", m.ID)
	}
	if m.Cardinality != "multi" {
		t.Fatalf("cardinality: %s", m.Cardinality)
	}
	if len(m.Contributes.Channels) != 1 || m.Contributes.Channels[0].Kind != "telegram" {
		t.Fatalf("channel contribution: %+v", m.Contributes.Channels)
	}
	// Verify the ChannelContribution advertises threading — necessary for
	// the Conversation model to engage when it lands.
	if !m.Contributes.Channels[0].SupportsThreading {
		t.Fatal("telegram channel must advertise SupportsThreading=true")
	}
	// Capabilities must match the pre-migration manifest so existing
	// permission policies keep working unchanged.
	want := map[string]bool{"network.outgoing": true, "filesystem.read": true}
	if len(m.Capabilities) != len(want) {
		t.Fatalf("capabilities: %v", m.Capabilities)
	}
	for _, cap := range m.Capabilities {
		if !want[cap] {
			t.Fatalf("unexpected capability %s", cap)
		}
	}
}

func TestChannelSend_UsesPerConnectionToken(t *testing.T) {
	var gotPaths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &Channel{connectionID: "conn-a", token: "tok-a", apiBase: srv.URL}
	if err := c.Send(context.Background(), "123", plugins.OutboundMessage{Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	c2 := &Channel{connectionID: "conn-b", token: "tok-b", apiBase: srv.URL}
	if err := c2.Send(context.Background(), "456", plugins.OutboundMessage{Text: "world"}); err != nil {
		t.Fatalf("Send b: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotPaths) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(gotPaths))
	}
	if !strings.Contains(gotPaths[0], "/bottok-a/sendMessage") {
		t.Fatalf("first request path: %s", gotPaths[0])
	}
	if !strings.Contains(gotPaths[1], "/bottok-b/sendMessage") {
		t.Fatalf("second request path: %s", gotPaths[1])
	}
}

func TestChannelSend_RoutesImageThroughSendPhoto(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	var lastFormCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		lastFormCT = r.Header.Get("Content-Type")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &Channel{connectionID: "conn", token: "tok", apiBase: srv.URL}
	err := c.Send(context.Background(), "123", plugins.OutboundMessage{
		Text: "look at this",
		Attachments: []plugins.Attachment{{
			Kind:     plugins.AttachmentImage,
			Filename: "cat.png",
			Data:     []byte{0x89, 0x50, 0x4e, 0x47}, // PNG magic — content unimportant
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d (%v)", len(hits), hits)
	}
	if !strings.Contains(hits[0], "/sendPhoto") {
		t.Fatalf("expected sendPhoto endpoint, got %s", hits[0])
	}
	if !strings.HasPrefix(lastFormCT, "multipart/form-data") {
		t.Fatalf("expected multipart upload, got Content-Type %q", lastFormCT)
	}
}

func TestChannelSend_RoutesVoiceThroughSendVoice(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := &Channel{connectionID: "conn", token: "tok", apiBase: srv.URL}
	err := c.Send(context.Background(), "123", plugins.OutboundMessage{
		Attachments: []plugins.Attachment{{
			Kind:    plugins.AttachmentAudio,
			Caption: "TTS reply",
			Data:    []byte{0x4f, 0x67, 0x67, 0x53}, // OggS magic
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(hit, "/sendVoice") {
		t.Fatalf("expected sendVoice endpoint, got %s", hit)
	}
}

func TestChannelSend_URLAttachmentSkipsMultipart(t *testing.T) {
	var lastCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := &Channel{connectionID: "conn", token: "tok", apiBase: srv.URL}
	err := c.Send(context.Background(), "123", plugins.OutboundMessage{
		Attachments: []plugins.Attachment{{
			Kind: plugins.AttachmentImage,
			URL:  "https://example.com/cat.png",
		}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// URL path uses JSON, not multipart.
	if !strings.HasPrefix(lastCT, "application/json") {
		t.Fatalf("URL attachment should send JSON, got %q", lastCT)
	}
}

func TestEndpointForAttachment_FallsBackToDocument(t *testing.T) {
	endpoint, field := telegramEndpointForAttachment(plugins.AttachmentKind("unknown"))
	if endpoint != "sendDocument" || field != "document" {
		t.Fatalf("unknown kind should fall back to sendDocument, got (%s, %s)", endpoint, field)
	}
}

func TestChannelSend_SurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()

	c := &Channel{connectionID: "conn", token: "tok", apiBase: srv.URL}
	err := c.Send(context.Background(), "999", plugins.OutboundMessage{Text: "test"})
	if err == nil {
		t.Fatal("expected API error")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("expected error to include description, got: %v", err)
	}
}

func TestResolveChannelAssistant_PrefersPrimary(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := db.NewConnectionRepository(database)
	bindRepo := db.NewAssistantBindingRepository(database)

	// Seed a connection and two assistants — one primary, one secondary.
	seedAssistant(t, database, "asst-primary")
	seedAssistant(t, database, "asst-other")
	_ = connRepo.Create(&domain.Connection{ID: "conn-1", PluginID: PluginID, Name: "bot", Enabled: true})

	_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID:  "asst-other",
		ConnectionID: "conn-1",
		Role:         domain.BindingRoleChannel,
		Enabled:      true,
	})
	_ = bindRepo.Upsert(&domain.AssistantConnectionBinding{
		AssistantID:  "asst-primary",
		ConnectionID: "conn-1",
		Role:         domain.BindingRoleChannel,
		Enabled:      true,
		IsPrimary:    true,
	})

	p := &Plugin{connections: connRepo, bindings: bindRepo}
	got, err := p.resolveChannelAssistant("conn-1")
	if err != nil {
		t.Fatalf("resolveChannelAssistant: %v", err)
	}
	if got != "asst-primary" {
		t.Fatalf("should prefer primary, got %s", got)
	}
}

func TestResolveChannelAssistant_ErrorsWithoutBinding(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	connRepo := db.NewConnectionRepository(database)
	bindRepo := db.NewAssistantBindingRepository(database)
	_ = connRepo.Create(&domain.Connection{ID: "orphan", PluginID: PluginID, Name: "bot", Enabled: true})

	p := &Plugin{connections: connRepo, bindings: bindRepo}
	if _, err := p.resolveChannelAssistant("orphan"); err == nil {
		t.Fatal("expected error for unbound connection")
	}
}

func seedAssistant(t *testing.T, database *db.DB, id string) {
	t.Helper()
	_, err := database.Exec(
		`INSERT INTO assistants (id, name, role, system_prompt, capabilities, channels, contexts, memory_policy, permission_policy)
		 VALUES (?, 'Test', 'assistant', '', '[]', '[]', '[]', '{}', '{}')`,
		id,
	)
	if err != nil {
		t.Fatalf("seed assistant %s: %v", id, err)
	}
}

func TestResolveBotToken_ReturnsErrorForMissingCredential(t *testing.T) {
	conn := &domain.Connection{ID: "c", PluginID: PluginID}
	p := &Plugin{secrets: newMemSecrets()}
	if _, err := p.resolveBotToken(conn); err == nil {
		t.Fatal("should error when credential_refs lacks bot_token")
	}
}

func TestResolveBotToken_DereferencesSecretURI(t *testing.T) {
	store := newMemSecrets()
	_ = store.Put("telegram/c/bot_token", "real-token-123")
	conn := &domain.Connection{
		ID:             "c",
		PluginID:       PluginID,
		CredentialRefs: map[string]string{"bot_token": "secret://telegram/c/bot_token"},
	}
	p := &Plugin{secrets: store}
	got, err := p.resolveBotToken(conn)
	if err != nil {
		t.Fatalf("resolveBotToken: %v", err)
	}
	if got != "real-token-123" {
		t.Fatalf("wrong token resolved: %s", got)
	}
}

func TestDataMigration_BackfillsTelegramConnections(t *testing.T) {
	database, cleanup := newTestDB(t)
	defer cleanup()

	seedAssistant(t, database, "asst-1")

	// Seed a pre-migration Telegram connector_configs row.
	legacy := `{"connections":[{"id":"conn-legacy","name":"Work","bot_token":"secret://telegram/conn-legacy/bot_token","default_assistant_id":"asst-1","enabled":true}]}`
	_, err := database.Exec(
		`INSERT INTO connector_configs (connector_name, config, enabled, updated_at)
		 VALUES ('telegram', ?, 1, CURRENT_TIMESTAMP)`,
		legacy,
	)
	if err != nil {
		t.Fatalf("seed connector_configs: %v", err)
	}

	// Re-run migration 11 manually. The Migrate() earlier already ran it
	// with an empty connector_configs table (no-op); re-executing it now
	// processes the seeded row. In production the order is reversed
	// (connector_configs predates the plugin tables), so this proves the
	// migration is idempotent and correct regardless of ordering.
	upSQL, err := os.ReadFile("../../storage/db/migrations/000011_migrate_telegram_to_plugins.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := database.Exec(string(upSQL)); err != nil {
		t.Fatalf("re-run migration 11: %v", err)
	}

	connRepo := db.NewConnectionRepository(database)
	bindRepo := db.NewAssistantBindingRepository(database)

	got, err := connRepo.GetByID("conn-legacy")
	if err != nil {
		t.Fatalf("connection not migrated: %v", err)
	}
	if got.PluginID != PluginID || got.Name != "Work" || !got.Enabled {
		t.Fatalf("migrated connection wrong: %+v", got)
	}
	if got.CredentialRefs["bot_token"] != "secret://telegram/conn-legacy/bot_token" {
		t.Fatalf("credential ref lost: %+v", got.CredentialRefs)
	}

	binds, err := bindRepo.ListByConnection("conn-legacy")
	if err != nil {
		t.Fatalf("ListByConnection: %v", err)
	}
	if len(binds) != 1 {
		t.Fatalf("expected one binding, got %d", len(binds))
	}
	b := binds[0]
	if b.AssistantID != "asst-1" || b.Role != domain.BindingRoleChannel || !b.IsPrimary {
		t.Fatalf("binding shape wrong: %+v", b)
	}
}
