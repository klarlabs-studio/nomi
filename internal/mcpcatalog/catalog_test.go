package mcpcatalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseNomiEnvelope(t *testing.T) {
	raw := `{
		"schema_version": 1,
		"presets": [
			{
				"id": "sqlite",
				"label": "SQLite",
				"description": "Query a local SQLite DB",
				"suggested_name": "SQLite",
				"transport": "stdio",
				"category": "data",
				"runtime": "npx",
				"command": "npx",
				"args": "-y mcp-sqlite /path/to/db.sqlite",
				"setup_note": "Replace the DB path.",
				"doc_url": "https://example.com/sqlite",
				"ready_to_create": false
			}
		]
	}`
	presets, err := Parse([]byte(raw), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 1 {
		t.Fatalf("got %d presets", len(presets))
	}
	p := presets[0]
	if p.ID != "sqlite" || p.Source != "remote" || p.CatalogOrigin != "example.com" {
		t.Fatalf("unexpected preset: %+v", p)
	}
	if p.Command != "npx" || !strings.Contains(p.Args, "mcp-sqlite") {
		t.Fatalf("command/args: %q %q", p.Command, p.Args)
	}
}

func TestParseGooseCatalog(t *testing.T) {
	raw := `[
		{
			"id": "agentql-mcp",
			"name": "AgentQL",
			"description": "Transform unstructured web content",
			"command": "npx -y agentql-mcp",
			"link": "https://github.com/tinyfish-io/agentql-mcp",
			"installation_notes": "Install using npx.",
			"is_builtin": false,
			"endorsed": false,
			"environmentVariables": [
				{"name": "AGENTQL_API_KEY", "description": "API key", "required": true}
			]
		},
		{
			"id": "autovisualiser",
			"name": "Auto Visualiser",
			"command": "",
			"is_builtin": true,
			"endorsed": true,
			"environmentVariables": []
		},
		{
			"id": "dev.to",
			"name": "Dev.to",
			"description": "Dev.to articles",
			"url": "http://localhost:3000/mcp",
			"link": "https://github.com/nickytonline/dev-to-mcp",
			"installation_notes": "Run locally first.",
			"is_builtin": false,
			"type": "streamable-http",
			"environmentVariables": []
		}
	]`
	presets, err := Parse([]byte(raw), "raw.githubusercontent.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 2 {
		t.Fatalf("want 2 (skip builtin), got %d", len(presets))
	}
	byID := map[string]Preset{}
	for _, p := range presets {
		byID[p.ID] = p
	}
	aq := byID["agentql-mcp"]
	if aq.Command != "npx" || aq.Args != "-y agentql-mcp" {
		t.Fatalf("agentql command split: %+v", aq)
	}
	if aq.Runtime != "npx" || len(aq.EnvCredentials) != 1 {
		t.Fatalf("agentql meta: %+v", aq)
	}
	if aq.EnvCredentials[0].Key != "AGENTQL_API_KEY" || !aq.EnvCredentials[0].Required {
		t.Fatalf("env cred: %+v", aq.EnvCredentials)
	}
	if aq.ReadyToCreate {
		t.Fatal("secret preset should not be ready_to_create")
	}
	dev := byID["dev.to"]
	if dev.Transport != "http" || dev.Endpoint != "http://localhost:3000/mcp" {
		t.Fatalf("http preset: %+v", dev)
	}
	if dev.Category != "remote" || dev.Runtime != "http" {
		t.Fatalf("http category/runtime: %+v", dev)
	}
}

func TestParseBareNomiArray(t *testing.T) {
	raw := `[{"id":"x","label":"X","description":"d","transport":"stdio","category":"local","runtime":"uvx","command":"uvx","args":"foo","setup_note":"n","doc_url":"https://x.test","ready_to_create":true}]`
	presets, err := Parse([]byte(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 1 || presets[0].ID != "x" {
		t.Fatalf("%+v", presets)
	}
}

func TestParseRejectsBadSchema(t *testing.T) {
	_, err := Parse([]byte(`{"schema_version":99,"presets":[]}`), "")
	if err == nil {
		t.Fatal("expected schema error")
	}
}

func TestClientFetchAndCache(t *testing.T) {
	catalog := []map[string]any{{
		"id": "mem", "name": "Memory", "description": "kg",
		"command": "npx -y @modelcontextprotocol/server-memory",
		"link":    "https://example.com", "installation_notes": "npx",
		"is_builtin": false, "environmentVariables": []any{},
	}}
	body, _ := json.Marshal(catalog)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.Client())
	if err := c.SetURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p1, err := c.Presets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 1 || p1[0].ID != "mem" {
		t.Fatalf("%+v", p1)
	}
	p2, err := c.Presets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("cache miss: hits=%d", hits)
	}
	if len(p2) != 1 {
		t.Fatalf("%+v", p2)
	}

	// Force refresh
	if _, err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("refresh hits=%d", hits)
	}
	st := c.Status()
	if st.PresetCount != 1 || st.URL != srv.URL || st.FetchedAt.IsZero() {
		t.Fatalf("%+v", st)
	}
	if time.Since(st.FetchedAt) > time.Minute {
		t.Fatalf("fetched_at weird: %v", st.FetchedAt)
	}
}

func TestValidateCatalogURL(t *testing.T) {
	if err := ValidateCatalogURL("https://hub.example/index.json"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCatalogURL("http://127.0.0.1:9/c.json"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCatalogURL("ftp://x"); err == nil {
		t.Fatal("expected scheme error")
	}
	if err := ValidateCatalogURL("not-a-url"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSplitCommand(t *testing.T) {
	bin, args := splitCommand("npx -y foo bar")
	if bin != "npx" || args != "-y foo bar" {
		t.Fatalf("%q %q", bin, args)
	}
}
