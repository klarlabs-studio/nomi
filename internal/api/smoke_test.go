package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/felixgeelhaar/nomi/internal/connectors"
	"github.com/felixgeelhaar/nomi/internal/domain"
	"github.com/felixgeelhaar/nomi/internal/events"
	"github.com/felixgeelhaar/nomi/internal/memory"
	"github.com/felixgeelhaar/nomi/internal/permissions"
	"github.com/felixgeelhaar/nomi/internal/runtime"
	"github.com/felixgeelhaar/nomi/internal/secrets"
	"github.com/felixgeelhaar/nomi/internal/storage/db"
	"github.com/felixgeelhaar/nomi/internal/tools"
)

// memorySecretStore is an in-memory secrets.Store for tests. It's here
// rather than in the secrets package because only the API tests need it as
// an injected dependency, and promoting a test helper into a production
// package just for test convenience would muddy the API.
type memorySecretStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemorySecretStore() *memorySecretStore {
	return &memorySecretStore{data: make(map[string]string)}
}

func (s *memorySecretStore) Put(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func (s *memorySecretStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}

func (s *memorySecretStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

// testAuthToken is exactly 64 hex characters — the same length as what
// LoadOrGenerateAuthToken writes in production (32 bytes hex-encoded).
const testAuthToken = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// harness is everything a handler test needs: a router wired to a temp
// SQLite, the runtime that backs it, and a cleanup func.
type harness struct {
	router     *gin.Engine
	runtime    *runtime.Runtime
	db         *db.DB
	events     *events.EventBus
	secrets    secrets.Store
	connectors *connectors.Registry
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.New(db.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	bus := events.NewEventBus(db.NewEventRepository(database))
	permEngine := permissions.NewEngine()
	approvalStore := db.NewApprovalRepository(database)
	approvalMgr := permissions.NewApprovalManager(approvalStore, bus)

	toolReg := tools.NewRegistry()
	if err := tools.RegisterCoreTools(toolReg); err != nil {
		t.Fatalf("register tools: %v", err)
	}
	toolExec := tools.NewExecutor(toolReg)

	memMgr := memory.NewManager(db.NewMemoryRepository(database))
	rt := runtime.NewRuntime(database, bus, permEngine, approvalMgr, toolExec, memMgr, runtime.DefaultConfig())

	connReg := connectors.NewRegistry()
	secretStore := newMemorySecretStore()

	router := NewRouter(RouterConfig{
		Runtime:    rt,
		DB:         database,
		EventBus:   bus,
		Approvals:  approvalMgr,
		Memory:     memMgr,
		Tools:      toolReg,
		Connectors: connReg,
		Secrets:    secretStore,
		AuthToken:  testAuthToken,
	})

	h := &harness{
		router:     router,
		runtime:    rt,
		db:         database,
		events:     bus,
		secrets:    secretStore,
		connectors: connReg,
	}
	t.Cleanup(func() {
		rt.Shutdown()
		database.Close()
	})
	return h
}

// do sends a request and returns the recorded response. It attaches the
// bearer token to everything by default so each individual test doesn't
// have to remember.
func (h *harness) do(method, path string, body any) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// --- auth tests --------------------------------------------------------

func TestHealthIsPublic(t *testing.T) {
	h := newHarness(t)
	// No Authorization header.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/health unauthed should 200, got %d", w.Code)
	}
}

func TestMetricsIsPublicAndExposesNomiSeries(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics unauthed should 200, got %d", w.Code)
	}
	body := w.Body.String()
	// nomi_runs_created_total is a plain Counter, so it emits HELP +
	// TYPE + a sample even with zero increments. Vec metrics
	// (Histograms / Counters) only emit once a label combination has
	// been observed; we don't drive runs in this smoke test, so we
	// assert only the registered baseline. A regression in the
	// metrics registry shows up here as a 404 or a body without the
	// nomi_ prefix.
	if !strings.Contains(body, "nomi_runs_created_total") {
		t.Errorf("/metrics output missing nomi_runs_created_total; body was:\n%s", body)
	}
}

func TestVersionIsPublic(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/version unauthed should 200, got %d", w.Code)
	}
	var body struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildDate string `json:"build_date"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /version body: %v", err)
	}
	// Default values still populate the response — auto-updater + About
	// panel rely on the keys being present even when ldflags weren't
	// applied (e.g. `go run` builds).
	if body.Version == "" || body.Commit == "" || body.BuildDate == "" {
		t.Fatalf("expected non-empty fields, got %+v", body)
	}
}

func TestMissingBearerTokenRejected(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/runs", nil)
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestInvalidBearerTokenRejected(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/runs", nil)
	req.Header.Set("Authorization", "Bearer wrong-token-xxx")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for wrong token, got %d", w.Code)
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodOptions, "/runs", nil)
	req.Header.Set("Origin", "tauri://localhost")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 preflight, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "tauri://localhost" {
		t.Fatalf("CORS origin echo = %q, want tauri://localhost", got)
	}
}

func TestCORSUnknownOriginNotEchoed(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodOptions, "/runs", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unknown origin should not receive Allow-Origin; got %q", got)
	}
}

// --- assistant + run CRUD -----------------------------------------------

func createAssistant(t *testing.T, h *harness) string {
	t.Helper()
	w := h.do(http.MethodPost, "/assistants", map[string]any{
		"name":          "tester",
		"role":          "dev",
		"system_prompt": "you are a test assistant",
		"permission_policy": map[string]any{
			"rules": []map[string]any{
				{"capability": "filesystem.read", "mode": "allow"},
			},
		},
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create assistant: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if out.ID == "" {
		t.Fatalf("no id in response body: %s", w.Body.String())
	}
	return out.ID
}

func TestCreateAndListAssistant(t *testing.T) {
	h := newHarness(t)
	id := createAssistant(t, h)

	w := h.do(http.MethodGet, "/assistants", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), id) {
		t.Fatalf("list did not include %s: %s", id, w.Body.String())
	}
}

func TestListAssistantTemplates(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/assistants/templates", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var out struct {
		Templates []domain.AssistantDefinition `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal templates: %v", err)
	}
	if len(out.Templates) != 7 {
		t.Fatalf("want 7 templates, got %d", len(out.Templates))
	}
}

func TestGetAssistantNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/assistants/no-such-id", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestCreateAssistantBadJSON(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/assistants", strings.NewReader("not-json"))
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestCreateRunHappyPath(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)

	w := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "do the thing",
		"assistant_id": aid,
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create run: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal run: %v", err)
	}
	if out.ID == "" {
		t.Fatal("run response missing id")
	}
}

// --- events list + SSE stream -------------------------------------------

func TestListEventsEmpty(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/events", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var out struct {
		Events []domain.Event `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Events) != 0 {
		t.Fatalf("fresh DB should have no events: %d", len(out.Events))
	}
}

func TestAuditExportJSON(t *testing.T) {
	h := newHarness(t)
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w := h.do(http.MethodGet, "/audit/export?from="+from+"&to="+to+"&format=json", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("audit export: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"signature":`) {
		t.Fatalf("expected signature in export: %s", w.Body.String())
	}
}

func TestAuditPrune(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/audit/prune", map[string]any{"days": 30})
	if w.Code != http.StatusOK {
		t.Fatalf("audit prune: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"pruned"`) {
		t.Fatalf("expected pruned status: %s", w.Body.String())
	}
}

// TestSSEStreamDeliversPublishedEvent verifies the /events/stream endpoint
// forwards a newly-published event to the connected client within 500ms. A
// real HTTP server is used because httptest.ResponseRecorder cannot stream.
func TestSSEStreamDeliversPublishedEvent(t *testing.T) {
	h := newHarness(t)
	// Events have a FK to runs(id); create a real run first so Publish
	// isn't silently rejected by SQLite's foreign_keys pragma.
	aid := createAssistant(t, h)
	runW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "sse-test",
		"assistant_id": aid,
	})
	if runW.Code != http.StatusCreated && runW.Code != http.StatusOK {
		t.Fatalf("create run: %d %s", runW.Code, runW.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(runW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	runID := created.ID

	srv := httptest.NewServer(h.router)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/stream?run_id="+runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAuthToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}

	// Publish a new event for the same run on a background goroutine so the
	// reader below can watch for it arriving on the stream.
	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := h.events.Publish(context.Background(), domain.EventStepStarted, runID, nil, map[string]any{
			"sse-marker": "delivered",
		}); err != nil {
			t.Logf("publish failed: %v", err)
		}
	}()

	// Pipe the SSE reader through a channel so the test body can apply a
	// real timeout — bufio.Reader.ReadString has no per-read deadline.
	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				lines <- strings.TrimSpace(line)
			}
			if err != nil {
				return
			}
		}
	}()

	deadline := time.After(1 * time.Second)
	var saw string
	for saw == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before event arrived")
			}
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if strings.Contains(payload, "sse-marker") {
					saw = payload
				}
			}
		case <-deadline:
			t.Fatal("did not see published event on SSE stream within 1s")
		}
	}
}

// --- provider secret redaction -----------------------------------------

func TestCreateProviderStashesSecret(t *testing.T) {
	h := newHarness(t)

	w := h.do(http.MethodPost, "/provider-profiles", map[string]any{
		"name":       "openai",
		"type":       "remote",
		"endpoint":   "https://api.example.com",
		"model_ids":  []string{"gpt-4"},
		"secret_ref": "sk-plaintext-goes-in-not-out",
		"enabled":    true,
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create provider: %d %s", w.Code, w.Body.String())
	}

	// Response must not leak the plaintext or the secret URI.
	body := w.Body.String()
	if strings.Contains(body, "sk-plaintext-goes-in-not-out") {
		t.Fatalf("plaintext leaked in response: %s", body)
	}
	if strings.Contains(body, "secret://") {
		t.Fatalf("secret reference URI leaked in response: %s", body)
	}
	if !strings.Contains(body, `"secret_configured":true`) {
		t.Fatalf("expected secret_configured flag: %s", body)
	}

	// The stashed value must be retrievable from the injected store.
	var count int
	for _, v := range h.secrets.(*memorySecretStore).data {
		if v == "sk-plaintext-goes-in-not-out" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected secret stashed exactly once; found %d matches", count)
	}
}

func TestUpdateProviderPreservesSecretWhenOmitted(t *testing.T) {
	h := newHarness(t)

	createW := h.do(http.MethodPost, "/provider-profiles", map[string]any{
		"name":       "openai",
		"type":       "remote",
		"endpoint":   "https://api.example.com",
		"model_ids":  []string{"gpt-4"},
		"secret_ref": "original-secret",
		"enabled":    true,
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	snapshotBefore := copyMap(h.secrets.(*memorySecretStore).data)

	// Update WITHOUT secret_ref — the stored secret must remain intact.
	updateW := h.do(http.MethodPut, "/provider-profiles/"+created.ID, map[string]any{
		"name":      "openai-renamed",
		"type":      "remote",
		"endpoint":  "https://api.example.com",
		"model_ids": []string{"gpt-4"},
		"enabled":   true,
	})
	if updateW.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updateW.Code, updateW.Body.String())
	}

	snapshotAfter := copyMap(h.secrets.(*memorySecretStore).data)
	if !mapsEqual(snapshotBefore, snapshotAfter) {
		t.Fatalf("update without secret_ref mutated the store: before=%v after=%v", snapshotBefore, snapshotAfter)
	}
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// --- misc smoke ---------------------------------------------------------

func TestEventsTypeFilter(t *testing.T) {
	h := newHarness(t)
	// events.run_id has an FK to runs(id); create a real run first.
	aid := createAssistant(t, h)
	runW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "events-filter",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(runW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if _, err := h.events.Publish(context.Background(), domain.EventStepStarted, created.ID, nil, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := h.events.Publish(context.Background(), domain.EventStepCompleted, created.ID, nil, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}

	w := h.do(http.MethodGet, "/events?type=step.completed&run_id="+created.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list events: %d", w.Code)
	}
	var out struct {
		Events []domain.Event `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) == 0 {
		t.Fatal("expected at least one step.completed event")
	}
	for _, e := range out.Events {
		if e.Type != domain.EventStepCompleted {
			t.Fatalf("filter leaked type %s", e.Type)
		}
	}
}

func TestConnectorConfigRedactsBotToken(t *testing.T) {
	h := newHarness(t)
	// Register a Telegram connector so the config endpoints see it.
	conn := connectors.NewTelegramConnector(
		connectors.TelegramConfig{},
		h.runtime,
		db.NewConnectorConfigRepository(h.db),
		h.secrets,
	)
	if err := h.connectors.Register(conn); err != nil {
		t.Fatalf("register telegram connector: %v", err)
	}

	// Write a token through the handler — it should be stashed, not stored plaintext.
	putW := h.do(http.MethodPut, "/connectors/telegram/config", map[string]any{
		"config": map[string]any{
			"connections": []map[string]any{
				{"id": "conn-1", "name": "b1", "bot_token": "SECRETTOKEN123", "enabled": true},
			},
		},
		"enabled": true,
	})
	if putW.Code != http.StatusOK {
		t.Fatalf("put config: %d %s", putW.Code, putW.Body.String())
	}

	// List — response must not contain the raw token or the secret URI.
	getW := h.do(http.MethodGet, "/connectors/configs", nil)
	body := getW.Body.String()
	if strings.Contains(body, "SECRETTOKEN123") {
		t.Fatalf("raw token leaked in connector list: %s", body)
	}
	if strings.Contains(body, "secret://") {
		t.Fatalf("secret URI leaked in connector list: %s", body)
	}
	if !strings.Contains(body, `"bot_token_configured":true`) {
		t.Fatalf("missing bot_token_configured flag: %s", body)
	}
}

func TestInvalidJSONPost(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/assistants", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 on malformed JSON, got %d", w.Code)
	}
}

// sanity: the test bearer token string is the length LoadOrGenerateAuthToken
// writes. If the length changes, the auth middleware won't accept it.
func TestAuthTokenLengthSanity(t *testing.T) {
	if len(testAuthToken) != 64 {
		t.Fatalf("test token must be 64 hex chars like the real one; len=%d", len(testAuthToken))
	}
}

// --- memory CRUD ---

func TestMemoryCreateAndList(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/memory", map[string]any{
		"content": "remember this",
		"scope":   "workspace",
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create memory: %d %s", w.Code, w.Body.String())
	}

	listW := h.do(http.MethodGet, "/memory", nil)
	if listW.Code != http.StatusOK {
		t.Fatalf("list memory: %d", listW.Code)
	}
	if !strings.Contains(listW.Body.String(), "remember this") {
		t.Fatalf("list body missing content: %s", listW.Body.String())
	}
}

func TestMemoryDelete(t *testing.T) {
	h := newHarness(t)
	createW := h.do(http.MethodPost, "/memory", map[string]any{
		"content": "to delete",
		"scope":   "workspace",
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	delW := h.do(http.MethodDelete, "/memory/"+created.ID, nil)
	if delW.Code != http.StatusOK {
		t.Fatalf("delete: %d", delW.Code)
	}
	// Get after delete should 404.
	getW := h.do(http.MethodGet, "/memory/"+created.ID, nil)
	if getW.Code != http.StatusNotFound {
		t.Fatalf("want 404 after delete, got %d", getW.Code)
	}
}

// --- approvals list / get ---

func TestApprovalsListEmpty(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/approvals", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list approvals: %d", w.Code)
	}
}

// --- provider profile delete ---

func TestProviderProfileDelete(t *testing.T) {
	h := newHarness(t)
	createW := h.do(http.MethodPost, "/provider-profiles", map[string]any{
		"name":       "temp",
		"type":       "remote",
		"endpoint":   "https://example.com",
		"model_ids":  []string{"m"},
		"secret_ref": "sk-delete-me",
		"enabled":    true,
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	delW := h.do(http.MethodDelete, "/provider-profiles/"+created.ID, nil)
	if delW.Code != http.StatusOK {
		t.Fatalf("delete provider: %d %s", delW.Code, delW.Body.String())
	}
}

// --- LLM default settings ---

func TestLLMDefaultSettings(t *testing.T) {
	h := newHarness(t)
	// GET before set returns empty strings (not an error).
	getW := h.do(http.MethodGet, "/settings/llm-default", nil)
	if getW.Code != http.StatusOK {
		t.Fatalf("get defaults: %d", getW.Code)
	}

	setW := h.do(http.MethodPut, "/settings/llm-default", map[string]any{
		"provider_id": "p1",
		"model_id":    "m1",
	})
	if setW.Code != http.StatusOK {
		t.Fatalf("set defaults: %d %s", setW.Code, setW.Body.String())
	}

	// GET round-trips the same values.
	getW2 := h.do(http.MethodGet, "/settings/llm-default", nil)
	if !strings.Contains(getW2.Body.String(), `"provider_id":"p1"`) {
		t.Fatalf("default provider not round-tripped: %s", getW2.Body.String())
	}
}

func TestOnboardingCompleteSettings(t *testing.T) {
	h := newHarness(t)

	getW := h.do(http.MethodGet, "/settings/onboarding-complete", nil)
	if getW.Code != http.StatusOK {
		t.Fatalf("get onboarding status: %d", getW.Code)
	}
	if !strings.Contains(getW.Body.String(), `"complete":false`) {
		t.Fatalf("expected onboarding to default false: %s", getW.Body.String())
	}

	setW := h.do(http.MethodPut, "/settings/onboarding-complete", map[string]any{"complete": true})
	if setW.Code != http.StatusOK {
		t.Fatalf("set onboarding status: %d %s", setW.Code, setW.Body.String())
	}

	getW2 := h.do(http.MethodGet, "/settings/onboarding-complete", nil)
	if !strings.Contains(getW2.Body.String(), `"complete":true`) {
		t.Fatalf("expected onboarding to be true after set: %s", getW2.Body.String())
	}
}

func TestSafetyProfileSettings(t *testing.T) {
	h := newHarness(t)

	getW := h.do(http.MethodGet, "/settings/safety-profile", nil)
	if getW.Code != http.StatusOK {
		t.Fatalf("get safety profile: %d", getW.Code)
	}
	if !strings.Contains(getW.Body.String(), `"profile":"balanced"`) {
		t.Fatalf("expected default balanced profile: %s", getW.Body.String())
	}

	setW := h.do(http.MethodPut, "/settings/safety-profile", map[string]any{"profile": "cautious"})
	if setW.Code != http.StatusOK {
		t.Fatalf("set safety profile: %d %s", setW.Code, setW.Body.String())
	}

	getW2 := h.do(http.MethodGet, "/settings/safety-profile", nil)
	if !strings.Contains(getW2.Body.String(), `"profile":"cautious"`) {
		t.Fatalf("expected cautious profile after set: %s", getW2.Body.String())
	}
}

func TestCreateAssistantUsesSafetyProfileDefaults(t *testing.T) {
	h := newHarness(t)

	setW := h.do(http.MethodPut, "/settings/safety-profile", map[string]any{"profile": "balanced"})
	if setW.Code != http.StatusOK {
		t.Fatalf("set safety profile: %d %s", setW.Code, setW.Body.String())
	}

	w := h.do(http.MethodPost, "/assistants", map[string]any{
		"name":          "profile-defaults",
		"role":          "dev",
		"system_prompt": "you are a test assistant",
	})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create assistant: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"capability":"filesystem.read","mode":"allow"`) {
		t.Fatalf("expected balanced default policy in response: %s", w.Body.String())
	}
}

func TestApplySafetyProfileToAssistant(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)

	setW := h.do(http.MethodPut, "/settings/safety-profile", map[string]any{"profile": "fast"})
	if setW.Code != http.StatusOK {
		t.Fatalf("set safety profile: %d %s", setW.Code, setW.Body.String())
	}

	applyW := h.do(http.MethodPost, "/assistants/"+aid+"/apply-safety-profile", nil)
	if applyW.Code != http.StatusOK {
		t.Fatalf("apply safety profile: %d %s", applyW.Code, applyW.Body.String())
	}
	if !strings.Contains(applyW.Body.String(), `"profile":"fast"`) {
		t.Fatalf("expected fast profile in response: %s", applyW.Body.String())
	}
}

// --- connector list + statuses ---

func TestListConnectorsAndStatuses(t *testing.T) {
	h := newHarness(t)
	conn := connectors.NewTelegramConnector(
		connectors.TelegramConfig{},
		h.runtime,
		db.NewConnectorConfigRepository(h.db),
		h.secrets,
	)
	_ = h.connectors.Register(conn)

	listW := h.do(http.MethodGet, "/connectors", nil)
	if listW.Code != http.StatusOK {
		t.Fatalf("list: %d", listW.Code)
	}
	if !strings.Contains(listW.Body.String(), "telegram") {
		t.Fatalf("telegram not in list: %s", listW.Body.String())
	}

	statusesW := h.do(http.MethodGet, "/connectors/statuses", nil)
	if statusesW.Code != http.StatusOK {
		t.Fatalf("statuses: %d", statusesW.Code)
	}

	statusW := h.do(http.MethodGet, "/connectors/telegram/status", nil)
	if statusW.Code != http.StatusOK {
		t.Fatalf("status: %d", statusW.Code)
	}
}

// --- run retrieval + retry + delete ---

func TestRunGetListDelete(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)

	createW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g1",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	getW := h.do(http.MethodGet, "/runs/"+created.ID, nil)
	if getW.Code != http.StatusOK {
		t.Fatalf("get run: %d", getW.Code)
	}

	listW := h.do(http.MethodGet, "/runs", nil)
	if listW.Code != http.StatusOK {
		t.Fatalf("list: %d", listW.Code)
	}

	delW := h.do(http.MethodDelete, "/runs/"+created.ID, nil)
	if delW.Code != http.StatusOK {
		t.Fatalf("delete: %d", delW.Code)
	}
}

// --- assistant update + delete ---

func TestAssistantUpdateAndDelete(t *testing.T) {
	h := newHarness(t)
	id := createAssistant(t, h)

	updW := h.do(http.MethodPut, "/assistants/"+id, map[string]any{
		"name":          "updated-name",
		"role":          "dev",
		"system_prompt": "revised prompt",
	})
	if updW.Code != http.StatusOK {
		t.Fatalf("update: %d %s", updW.Code, updW.Body.String())
	}

	delW := h.do(http.MethodDelete, "/assistants/"+id, nil)
	if delW.Code != http.StatusOK {
		t.Fatalf("delete: %d", delW.Code)
	}
}

// --- approvals: resolve-not-found ---

func TestApprovalsResolveNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/approvals/no-such-id/resolve", map[string]any{
		"approved": true,
	})
	if w.Code == http.StatusOK {
		t.Fatal("expected non-200 for unknown approval id")
	}
}

// --- provider profile update-not-found ---

func TestProviderProfileUpdateNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPut, "/provider-profiles/no-such", map[string]any{
		"name":      "n",
		"type":      "remote",
		"model_ids": []string{"m"},
		"enabled":   true,
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown provider id; got %d", w.Code)
	}
}

// --- provider profile get-not-found ---

func TestProviderProfileGetNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/provider-profiles/no-such", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404; got %d", w.Code)
	}
}

// --- tool preview endpoint ---

func TestToolsFolderContextPreview(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	w := h.do(http.MethodPost, "/tools/filesystem.context/preview", map[string]any{
		"path":      dir,
		"max_depth": 2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"path"`) {
		t.Fatalf("preview body missing path field: %s", w.Body.String())
	}
}

// --- run retry (terminal state only) ---

func TestRunRetryRequiresTerminalState(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)

	createW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// A freshly-created run is still active, so retry should refuse.
	w := h.do(http.MethodPost, "/runs/"+created.ID+"/retry", nil)
	if w.Code == http.StatusOK {
		t.Fatal("retry of non-terminal run should fail")
	}
}

// --- connector config update validates on bad body ---

func TestConnectorConfigUpdateBadJSON(t *testing.T) {
	h := newHarness(t)
	// Register so the route exists.
	conn := connectors.NewTelegramConnector(
		connectors.TelegramConfig{},
		h.runtime,
		db.NewConnectorConfigRepository(h.db),
		h.secrets,
	)
	_ = h.connectors.Register(conn)

	req := httptest.NewRequest(http.MethodPut, "/connectors/telegram/config", strings.NewReader("{bad json"))
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400; got %d", w.Code)
	}
}

// --- direct provider list (coverage) ---

func TestProviderProfileList(t *testing.T) {
	h := newHarness(t)
	_ = h.do(http.MethodPost, "/provider-profiles", map[string]any{
		"name":       "p1",
		"type":       "remote",
		"model_ids":  []string{"m"},
		"secret_ref": "sk-1",
		"enabled":    true,
	})
	w := h.do(http.MethodGet, "/provider-profiles", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"name":"p1"`) {
		t.Fatalf("list missing p1: %s", w.Body.String())
	}
	// The returned shape must not leak a secret_ref string.
	if strings.Contains(w.Body.String(), "secret_ref") {
		t.Fatalf("list leaked secret_ref field: %s", w.Body.String())
	}
}

// --- approval get-by-id ---

func TestApprovalGetByIDNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/approvals/no-such-id", nil)
	if w.Code == http.StatusOK {
		t.Fatal("expected error for unknown approval id")
	}
}

// --- run approvals subresource ---

func TestGetRunApprovalsEmpty(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)
	createW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w := h.do(http.MethodGet, "/runs/"+created.ID+"/approvals", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("run approvals: %d", w.Code)
	}
}

// --- plan approve / edit error paths ---

func TestApproveRunNoPlanReview(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)
	createW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createW.Body.Bytes(), &created)

	// Not in awaiting_approval state — approve should refuse.
	w := h.do(http.MethodPost, "/runs/"+created.ID+"/approve", nil)
	if w.Code == http.StatusOK {
		t.Fatal("approve should require awaiting_approval state")
	}
}

func TestApprovePlanNoPlanReview(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)
	createW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createW.Body.Bytes(), &created)

	w := h.do(http.MethodPost, "/runs/"+created.ID+"/plan/approve", nil)
	if w.Code == http.StatusOK {
		t.Fatal("plan approve should require plan_review state")
	}
}

func TestEditPlanBadBody(t *testing.T) {
	h := newHarness(t)
	aid := createAssistant(t, h)
	createW := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g",
		"assistant_id": aid,
	})
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createW.Body.Bytes(), &created)

	req := httptest.NewRequest(http.MethodPost, "/runs/"+created.ID+"/plan/edit", strings.NewReader("{bad"))
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400; got %d", w.Code)
	}
}

func TestCancelRunNoCancelableState(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/runs/no-such-id/cancel", nil)
	if w.Code == http.StatusOK {
		t.Fatal("cancel should fail for unknown run")
	}
}

func TestPauseRunUnknown(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/runs/no-such-id/pause", nil)
	if w.Code == http.StatusOK {
		t.Fatal("pause should fail for unknown run")
	}
}

func TestResumeRunUnknown(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/runs/no-such-id/resume", nil)
	if w.Code == http.StatusOK {
		t.Fatal("resume should fail for unknown run")
	}
}

// --- auth token generation ---

func TestLoadOrGenerateAuthTokenCreatesFile(t *testing.T) {
	dir := t.TempDir()
	tok, path, err := LoadOrGenerateAuthToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("expected 64-char hex token, got %d", len(tok))
	}
	if path == "" {
		t.Fatal("empty path returned")
	}

	// Second call reads the same file rather than regenerating.
	tok2, _, err := LoadOrGenerateAuthToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tok != tok2 {
		t.Fatal("token should persist across calls")
	}
}

// --- CORS vary header ---

func TestCORSSetsVaryHeader(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "tauri://localhost")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary should contain Origin; got %q", got)
	}
}

// --- run error paths ---

func TestRunGetNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/runs/no-such-id", nil)
	if w.Code == http.StatusOK {
		t.Fatal("expected error for unknown run id")
	}
}

func TestRunCreateMissingAssistant(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodPost, "/runs", map[string]any{
		"goal":         "g",
		"assistant_id": "no-such-assistant",
	})
	if w.Code == http.StatusOK || w.Code == http.StatusCreated {
		t.Fatalf("expected error; got %d", w.Code)
	}
}

func TestRunCreateBadBody(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400; got %d", w.Code)
	}
}

// --- assistant delete not-found ---

func TestAssistantDeleteNotFound(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodDelete, "/assistants/no-such", nil)
	// Repository delete of non-existent row is a no-op with most SQL
	// drivers; accept either 200 or 404 as long as it doesn't 500.
	if w.Code >= 500 {
		t.Fatalf("unexpected 5xx: %d", w.Code)
	}
}

// --- memory list with filters ---

func TestMemoryListFiltered(t *testing.T) {
	h := newHarness(t)
	_ = h.do(http.MethodPost, "/memory", map[string]any{
		"content": "alpha",
		"scope":   "workspace",
	})
	_ = h.do(http.MethodPost, "/memory", map[string]any{
		"content": "beta",
		"scope":   "profile",
	})
	w := h.do(http.MethodGet, "/memory?scope=profile", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "beta") {
		t.Fatalf("missing beta: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "alpha") {
		t.Fatalf("workspace scope leaked into profile filter: %s", w.Body.String())
	}
}

// --- events list with limit ---

func TestEventsListLimit(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/events?limit=5", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
}

// --- connector status unknown name ---

func TestConnectorStatusUnknown(t *testing.T) {
	h := newHarness(t)
	w := h.do(http.MethodGet, "/connectors/no-such/status", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404; got %d", w.Code)
	}
}

// assert fmt.Sprintf is used somewhere to keep go vet happy about the
// import declaration. In practice every route test would use it.
var _ = fmt.Sprintf
