package browser

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// stubInvoker records every CallTool call so tests can assert the
// Nomi-tool-name → MCP-tool-name + args translation. The replies
// map drives the response shape per MCP tool name.
type stubInvoker struct {
	mu      sync.Mutex
	calls   []stubCall
	replies map[string]json.RawMessage
	err     error
	closed  bool
}

type stubCall struct {
	Tool string
	Args map[string]interface{}
}

func (s *stubInvoker) CallTool(_ context.Context, name string, args map[string]interface{}) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{Tool: name, Args: args})
	if s.err != nil {
		return nil, s.err
	}
	if r, ok := s.replies[name]; ok {
		return r, nil
	}
	// Default: empty MCP result envelope.
	return json.RawMessage(`{"content":[]}`), nil
}

func (s *stubInvoker) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// fixture wires a plugin + connection + stub MCP invoker.
type fixture struct {
	plugin    *Plugin
	connID    string
	invoker   *stubInvoker
	connsRepo *db.ConnectionRepository
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(tmp, "test.db")})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conns := db.NewConnectionRepository(database)
	binds := db.NewAssistantBindingRepository(database)
	plug := NewPlugin(conns, binds, nil)
	invoker := &stubInvoker{replies: map[string]json.RawMessage{}}
	plug.SetClientFactory(func(_ context.Context, _ *domain.Connection) (mcpInvoker, error) {
		return invoker, nil
	})

	connID := "conn-browser-1"
	if err := conns.Create(&domain.Connection{
		ID:       connID,
		PluginID: PluginID,
		Name:     "test",
		Config: map[string]any{
			"scout_path":    "/usr/bin/scout",
			"allowed_hosts": []interface{}{"*"}, // permissive; per-host gate tests override explicitly
		},
		Enabled: true,
	}); err != nil {
		t.Fatalf("conn create: %v", err)
	}
	return &fixture{plugin: plug, connID: connID, invoker: invoker, connsRepo: conns}
}

// withAllowedHosts replaces the fixture connection's allowed_hosts
// with the given list. Used by per-host gating tests that need to
// see the deny path.
func (f *fixture) withAllowedHosts(t *testing.T, hosts []string) {
	t.Helper()
	c, _ := f.connsRepo.GetByID(f.connID)
	raw := make([]interface{}, len(hosts))
	for i, h := range hosts {
		raw[i] = h
	}
	c.Config["allowed_hosts"] = raw
	if err := f.connsRepo.Update(c); err != nil {
		t.Fatalf("update connection: %v", err)
	}
}

func (f *fixture) tool(name string) func(input map[string]interface{}) (map[string]interface{}, error) {
	for _, tl := range f.plugin.Tools() {
		if tl.Name() == name {
			t := tl
			return func(input map[string]interface{}) (map[string]interface{}, error) {
				return t.Execute(context.Background(), input)
			}
		}
	}
	return nil
}

// --- manifest ----------------------------------------------------

func TestManifest_DeclaresAllTools(t *testing.T) {
	m := NewPlugin(nil, nil, nil).Manifest()
	want := map[string]bool{
		"browser.navigate":        false,
		"browser.observe":         false,
		"browser.click":           false,
		"browser.type":            false,
		"browser.fill_form":       false,
		"browser.extract":         false,
		"browser.extract_table":   false,
		"browser.screenshot":      false,
		"browser.readable_text":   false,
		"browser.markdown":        false,
		"browser.wait_for":        false,
		"browser.has_element":     false,
		"browser.scroll_by":       false,
		"browser.dismiss_cookies": false,
		"browser.console_errors":  false,
	}
	for _, tc := range m.Contributes.Tools {
		want[tc.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q missing from manifest", name)
		}
	}
}

// --- tool dispatch ----------------------------------------------

func TestNavigate_RoutesToScoutNavigate(t *testing.T) {
	f := newFixture(t)
	f.invoker.replies["navigate"] = json.RawMessage(`{"content":[{"type":"text","text":"{\"url\":\"https://example.com\",\"title\":\"Example\"}"}]}`)
	out, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID,
		"url":           "https://example.com",
	})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if len(f.invoker.calls) != 1 || f.invoker.calls[0].Tool != "navigate" {
		t.Fatalf("expected one call to navigate, got %+v", f.invoker.calls)
	}
	if f.invoker.calls[0].Args["url"] != "https://example.com" {
		t.Fatalf("url not threaded: %+v", f.invoker.calls[0].Args)
	}
	if out["title"] != "Example" {
		t.Fatalf("expected JSON-decoded reply, got %+v", out)
	}
}

func TestNavigate_RequiresConnectionID(t *testing.T) {
	f := newFixture(t)
	_, err := f.tool("browser.navigate")(map[string]interface{}{
		"url": "https://example.com",
	})
	if err == nil {
		t.Fatal("expected error for missing connection_id")
	}
	if !strings.Contains(err.Error(), "connection_id is required") {
		t.Fatalf("error should mention connection_id: %v", err)
	}
}

func TestNavigate_RequiresEnabledConnection(t *testing.T) {
	f := newFixture(t)
	c, _ := f.connsRepo.GetByID(f.connID)
	c.Enabled = false
	_ = f.connsRepo.Update(c)
	_, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID, "url": "https://example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("expected disabled-connection error, got %v", err)
	}
}

func TestNavigate_RefusesUnboundAssistant(t *testing.T) {
	f := newFixture(t)
	_, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id":  f.connID,
		"__assistant_id": "asst-no-binding",
		"url":            "https://example.com",
	})
	if err == nil {
		t.Fatal("expected ConnectionNotBoundError")
	}
	if !errors.Is(err, plugins.ErrConnectionNotBound) {
		t.Fatalf("expected ErrConnectionNotBound, got %v", err)
	}
}

func TestType_PassesSelectorAndText(t *testing.T) {
	f := newFixture(t)
	if _, err := f.tool("browser.type")(map[string]interface{}{
		"connection_id": f.connID,
		"selector":      "#email",
		"text":          "alice@example.com",
	}); err != nil {
		t.Fatalf("type: %v", err)
	}
	got := f.invoker.calls[0]
	if got.Tool != "type" || got.Args["selector"] != "#email" || got.Args["text"] != "alice@example.com" {
		t.Fatalf("type args drift: %+v", got)
	}
}

func TestFillForm_PassesFieldsObject(t *testing.T) {
	f := newFixture(t)
	fields := map[string]interface{}{"Email": "alice@example.com", "Password": "secret"}
	if _, err := f.tool("browser.fill_form")(map[string]interface{}{
		"connection_id": f.connID,
		"fields":        fields,
	}); err != nil {
		t.Fatalf("fill_form: %v", err)
	}
	got := f.invoker.calls[0]
	if got.Tool != "fill_form_semantic" {
		t.Fatalf("expected MCP name fill_form_semantic, got %q", got.Tool)
	}
	gotFields, _ := got.Args["fields"].(map[string]interface{})
	if gotFields["Email"] != "alice@example.com" {
		t.Fatalf("fields not threaded: %+v", got.Args)
	}
}

func TestObserve_DecodesElementList(t *testing.T) {
	f := newFixture(t)
	// Scout's annotated_screenshot/observe returns a JSON list of
	// elements wrapped in the MCP envelope. Confirm we surface it as
	// a structured map.
	f.invoker.replies["observe"] = json.RawMessage(`{"content":[{"type":"text","text":"{\"elements\":[{\"label\":1,\"selector\":\"#submit\",\"text\":\"Go\"}]}"}]}`)
	out, err := f.tool("browser.observe")(map[string]interface{}{
		"connection_id": f.connID,
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	elements, ok := out["elements"].([]interface{})
	if !ok || len(elements) != 1 {
		t.Fatalf("expected elements list, got %+v", out)
	}
}

func TestScreenshot_PassesViewportFlags(t *testing.T) {
	f := newFixture(t)
	if _, err := f.tool("browser.screenshot")(map[string]interface{}{
		"connection_id": f.connID,
		"full_page":     true,
		"max_width":     float64(800),
		"quality":       float64(60),
	}); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	got := f.invoker.calls[0]
	if got.Tool != "screenshot" {
		t.Fatalf("MCP tool name = %q", got.Tool)
	}
	if got.Args["full_page"] != true || got.Args["max_width"] != float64(800) || got.Args["quality"] != float64(60) {
		t.Fatalf("screenshot args drift: %+v", got.Args)
	}
}

func TestExtract_TextFallback(t *testing.T) {
	// Scout extract returns plain text (not JSON) — confirm we
	// surface it under "text" rather than dropping it.
	f := newFixture(t)
	f.invoker.replies["extract"] = json.RawMessage(`{"content":[{"type":"text","text":"Hello, world!"}]}`)
	out, err := f.tool("browser.extract")(map[string]interface{}{
		"connection_id": f.connID,
		"selector":      "h1",
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if out["text"] != "Hello, world!" {
		t.Fatalf("extract should fall back to text wrapper, got %+v", out)
	}
}

func TestMCPCallError_Surfaces(t *testing.T) {
	f := newFixture(t)
	f.invoker.err = errors.New("scout: navigation refused")
	_, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID, "url": "https://example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "navigation refused") {
		t.Fatalf("expected MCP error to surface, got %v", err)
	}
}

// --- lifecycle ---------------------------------------------------

func TestStartStop_Idempotent(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.plugin.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.plugin.Start(ctx); err != nil {
		t.Fatalf("Start 2 should be no-op, got %v", err)
	}
	if err := f.plugin.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := f.plugin.Stop(); err != nil {
		t.Fatalf("Stop 2: %v", err)
	}
}

func TestStop_ClosesActiveClients(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.plugin.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Touch the connection to materialize the client.
	if _, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID, "url": "https://example.com",
	}); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := f.plugin.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !f.invoker.closed {
		t.Fatal("Stop should close the live MCP invoker")
	}
}

// --- decodeMCPResult ---------------------------------------------

func TestDecodeMCPResult_HandlesShapes(t *testing.T) {
	// Nil/empty
	if got := decodeMCPResult(nil); len(got) != 0 {
		t.Fatalf("nil should produce empty map, got %v", got)
	}
	// Standard text envelope with JSON payload
	got := decodeMCPResult(json.RawMessage(`{"content":[{"type":"text","text":"{\"k\":1}"}]}`))
	if got["k"] != float64(1) {
		t.Fatalf("JSON payload not decoded, got %v", got)
	}
	// Standard text envelope with plain text payload
	got = decodeMCPResult(json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`))
	if got["text"] != "hello" {
		t.Fatalf("plain text fallback failed, got %v", got)
	}
	// isError surfaces
	got = decodeMCPResult(json.RawMessage(`{"content":[{"type":"text","text":"{\"err\":1}"}],"isError":true}`))
	if got["is_error"] != true {
		t.Fatalf("isError not surfaced, got %v", got)
	}
}
