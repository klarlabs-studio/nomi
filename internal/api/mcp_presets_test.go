package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"go.klarlabs.de/nomi/internal/mcpcatalog"
	"go.klarlabs.de/nomi/internal/storage/db"
)

func TestMCPPresetCatalog_SetAndList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	catalog := []map[string]any{{
		"id": "remote-mem", "name": "Remote Memory", "description": "kg",
		"command": "npx -y @modelcontextprotocol/server-memory",
		"link":    "https://example.com", "installation_notes": "npx",
		"is_builtin": false, "environmentVariables": []any{},
	}}
	body, _ := json.Marshal(catalog)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)

	client := mcpcatalog.NewClient(upstream.Client())
	srv := NewMCPPresetServer(client, db.NewAppSettingsRepository(database))
	r := gin.New()
	r.GET("/mcp/presets", srv.ListMCPPresets)
	r.POST("/mcp/presets/refresh", srv.RefreshMCPPresets)
	r.GET("/settings/mcp-preset-catalog", srv.GetMCPPresetCatalog)
	r.PUT("/settings/mcp-preset-catalog", srv.SetMCPPresetCatalog)

	// Empty by default
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/mcp/presets", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list empty: %d %s", w.Code, w.Body.String())
	}
	var empty mcpPresetsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Presets) != 0 {
		t.Fatalf("want 0 presets, got %d", len(empty.Presets))
	}

	// Point at upstream Goose-shaped catalog
	setBody, _ := json.Marshal(map[string]string{"url": upstream.URL})
	req := httptest.NewRequest(http.MethodPut, "/settings/mcp-preset-catalog", bytes.NewReader(setBody))
	req.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w2.Code, w2.Body.String())
	}
	var setResp mcpCatalogSettingsResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &setResp); err != nil {
		t.Fatal(err)
	}
	if setResp.URL != upstream.URL || setResp.PresetCount != 1 {
		t.Fatalf("set resp: %+v", setResp)
	}
	if setResp.ExampleGooseURL == "" {
		t.Fatal("missing example goose url")
	}

	// Persisted in settings
	got := db.NewAppSettingsRepository(database).GetOrDefault(mcpcatalog.SettingKey, "")
	if got != upstream.URL {
		t.Fatalf("settings: %q", got)
	}

	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/mcp/presets", nil))
	if w3.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w3.Code, w3.Body.String())
	}
	var listed mcpPresetsResponse
	if err := json.Unmarshal(w3.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Presets) != 1 || listed.Presets[0].ID != "remote-mem" {
		t.Fatalf("%+v", listed.Presets)
	}
	if listed.Presets[0].Source != "remote" {
		t.Fatalf("source=%q", listed.Presets[0].Source)
	}

	// Clear
	clearBody, _ := json.Marshal(map[string]string{"url": ""})
	req4 := httptest.NewRequest(http.MethodPut, "/settings/mcp-preset-catalog", bytes.NewReader(clearBody))
	req4.Header.Set("Content-Type", "application/json")
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("clear: %d", w4.Code)
	}
	w5 := httptest.NewRecorder()
	r.ServeHTTP(w5, httptest.NewRequest(http.MethodGet, "/mcp/presets", nil))
	var cleared mcpPresetsResponse
	_ = json.Unmarshal(w5.Body.Bytes(), &cleared)
	if len(cleared.Presets) != 0 {
		t.Fatalf("cleared still has presets: %+v", cleared.Presets)
	}
}

func TestMCPPresetCatalog_RejectsBadURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_ = database.Migrate()

	srv := NewMCPPresetServer(mcpcatalog.NewClient(nil), db.NewAppSettingsRepository(database))
	r := gin.New()
	r.PUT("/settings/mcp-preset-catalog", srv.SetMCPPresetCatalog)

	body, _ := json.Marshal(map[string]string{"url": "ftp://nope"})
	req := httptest.NewRequest(http.MethodPut, "/settings/mcp-preset-catalog", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", w.Code, w.Body.String())
	}
}
