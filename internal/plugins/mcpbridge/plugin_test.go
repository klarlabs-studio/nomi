package mcpbridge

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcpclient "go.klarlabs.de/mcp/client"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/storage/db"
	"go.klarlabs.de/nomi/internal/tools"
)

type stubSession struct {
	tools   []mcpclient.ToolInfo
	calls   []stubCall
	result  *mcpclient.ToolResult
	listErr error
	callErr error
}

type stubCall struct {
	Name string
	Args any
}

func (s *stubSession) ListTools(context.Context) ([]mcpclient.ToolInfo, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tools, nil
}

func (s *stubSession) CallTool(_ context.Context, name string, arguments any) (*mcpclient.ToolResult, error) {
	s.calls = append(s.calls, stubCall{Name: name, Args: arguments})
	if s.callErr != nil {
		return nil, s.callErr
	}
	if s.result != nil {
		return s.result, nil
	}
	return &mcpclient.ToolResult{
		Content: []mcpclient.ContentItem{{Type: "text", Text: "ok:" + name}},
	}, nil
}

func (s *stubSession) Close() error { return nil }

func TestManifestAndStaticTools(t *testing.T) {
	p := NewPlugin(nil, nil, nil, nil)
	m := p.Manifest()
	if m.ID != PluginID {
		t.Fatalf("id = %q", m.ID)
	}
	if m.Cardinality != plugins.ConnectionMulti {
		t.Fatal("expected multi cardinality")
	}
	if len(m.Contributes.Tools) != 2 {
		t.Fatalf("tools = %d", len(m.Contributes.Tools))
	}
	got := p.Tools()
	if len(got) != 2 {
		t.Fatalf("Tools() = %d", len(got))
	}
	for _, tool := range got {
		if tool.Capability() != Capability {
			t.Errorf("%s capability = %s", tool.Name(), tool.Capability())
		}
	}
}

func TestSplitArgsAndSlugify(t *testing.T) {
	if got := splitArgs("run --dir /tmp"); len(got) != 3 || got[0] != "run" {
		t.Fatalf("splitArgs spaces: %v", got)
	}
	if got := splitArgs("a,b, c"); len(got) != 3 {
		t.Fatalf("splitArgs csv: %v", got)
	}
	if got := slugify("Filesystem MCP"); got != "filesystem_mcp" {
		t.Fatalf("slugify = %q", got)
	}
	if got := slugify("  "); got != "" {
		t.Fatalf("empty slug = %q", got)
	}
}

func TestDispatchCallAndDiscover(t *testing.T) {
	tmp := t.TempDir()
	database, err := db.New(db.Config{Path: filepath.Join(tmp, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	conns := db.NewConnectionRepository(database)
	binds := db.NewAssistantBindingRepository(database)
	reg := tools.NewRegistry()
	p := NewPlugin(conns, binds, nil, reg)
	stub := &stubSession{
		tools: []mcpclient.ToolInfo{
			{Name: "search", Description: "Search the corpus"},
			{Name: "fetch", Description: "Fetch a document"},
		},
	}
	p.SetSessionFactory(func(context.Context, *domain.Connection) (mcpSession, error) {
		return stub, nil
	})

	conn := &domain.Connection{
		ID:       "conn-mcp-1",
		PluginID: PluginID,
		Name:     "Docs",
		Enabled:  true,
		Config:   map[string]any{"transport": "stdio", "command": "fake-mcp"},
	}
	if err := conns.Create(conn); err != nil {
		t.Fatal(err)
	}

	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	names := map[string]bool{}
	for _, n := range reg.Names() {
		names[n] = true
	}
	if !names["mcp.docs.search"] || !names["mcp.docs.fetch"] {
		t.Fatalf("discovered names: %v", reg.Names())
	}

	list := &dispatchTool{plugin: p, name: "mcp.list_tools", kind: dispatchList}
	out, err := list.Execute(context.Background(), map[string]interface{}{"connection_id": conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	toolsOut, _ := out["tools"].([]map[string]interface{})
	if len(toolsOut) != 2 {
		t.Fatalf("list tools = %#v", out)
	}

	call := &dispatchTool{plugin: p, name: "mcp.call", kind: dispatchCall}
	out, err = call.Execute(context.Background(), map[string]interface{}{
		"connection_id": conn.ID,
		"tool":          "search",
		"arguments":     map[string]interface{}{"q": "nomi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "ok:search" {
		t.Fatalf("call result = %#v", out)
	}
	if len(stub.calls) != 1 || stub.calls[0].Name != "search" {
		t.Fatalf("calls = %#v", stub.calls)
	}

	discovered, err := reg.Get("mcp.docs.search")
	if err != nil {
		t.Fatal(err)
	}
	out, err = discovered.Execute(context.Background(), map[string]interface{}{"q": "n"})
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "ok:search" {
		t.Fatalf("discovered execute = %#v", out)
	}

	search, err := reg.Get("mcp.docs.search")
	if err != nil {
		t.Fatal(err)
	}
	if search.Capability() != "mcp.docs.search" {
		t.Fatalf("search capability = %q, want per-tool mcp.docs.search", search.Capability())
	}
	fetch, err := reg.Get("mcp.docs.fetch")
	if err != nil {
		t.Fatal(err)
	}
	if fetch.Capability() != "mcp.docs.fetch" {
		t.Fatalf("fetch capability = %q, want per-tool mcp.docs.fetch", fetch.Capability())
	}
	if search.Capability() == fetch.Capability() {
		t.Fatal("distinct tools must not share a capability")
	}

	health, ok := p.ConnectionHealth(conn.ID)
	if !ok || len(health.DiscoveredTools) != 2 {
		t.Fatalf("health discovered = %#v ok=%v", health, ok)
	}
}

func TestDiscoveredToolKeepsInputSchema(t *testing.T) {
	reg := tools.NewRegistry()
	p := NewPlugin(nil, nil, nil, reg)
	conn := &domain.Connection{ID: "c1", Name: "Docs", Enabled: true}
	p.syncDiscoveredTools(conn, []mcpclient.ToolInfo{{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []any{"path"},
		},
	}})
	tool, err := reg.Get("mcp.docs.read_file")
	if err != nil {
		t.Fatal(err)
	}
	sp, ok := tool.(tools.SchemaProvider)
	if !ok {
		t.Fatal("expected SchemaProvider")
	}
	sum := tools.CompactSchemaSummary(sp.InputSchema())
	if sum != "Args: path (string)" {
		t.Fatalf("summary = %q", sum)
	}
	ex := tools.NewExecutor(reg)
	if got := ex.SchemaSummaryFor("mcp.docs.read_file"); got != sum {
		t.Fatalf("SchemaSummaryFor = %q", got)
	}
}

func TestFlattenStructuredContent(t *testing.T) {
	raw := json.RawMessage(`{"hits":1}`)
	out := flattenToolResult(&mcpclient.ToolResult{
		Content:           []mcpclient.ContentItem{{Type: "text", Text: "hi"}},
		StructuredContent: raw,
	})
	if out["text"] != "hi" {
		t.Fatalf("text = %v", out["text"])
	}
	if out["structured"] == nil {
		t.Fatal("expected structured")
	}
}
