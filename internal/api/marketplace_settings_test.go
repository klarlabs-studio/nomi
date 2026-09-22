package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"go.klarlabs.de/nomi/internal/plugins/hub"
	"go.klarlabs.de/nomi/internal/storage/db"
)

func TestMarketplaceCatalogSettings_SetAndRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	catJSON, _ := json.Marshal(hub.Catalog{
		SchemaVersion: hub.SchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Entries: []hub.Entry{{
			PluginID:      "com.example.echo",
			Name:          "Echo",
			LatestVersion: "0.1.0",
			Capabilities:  []string{"echo.echo"},
			SHA256:        "deadbeef",
			BundleURL:     "https://example.invalid/echo.nomi-plugin",
		}},
	})
	signed, err := hub.SignCatalog(rootPriv, catJSON)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(signed)
	}))
	t.Cleanup(srv.Close)

	client, _ := hub.NewClient(rootPub, srv.Client())
	provider := hub.NewCachedProvider(client, "")
	apiSrv := NewMarketplaceCatalogServer(provider, db.NewAppSettingsRepository(database))
	r := gin.New()
	r.GET("/settings/marketplace-catalog", apiSrv.GetMarketplaceCatalogSettings)
	r.PUT("/settings/marketplace-catalog", apiSrv.SetMarketplaceCatalogSettings)
	r.POST("/plugins/marketplace/refresh", apiSrv.RefreshMarketplaceCatalog)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings/marketplace-catalog", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	var got marketplaceCatalogSettingsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Configured || got.EffectiveURL != hub.DefaultCatalogURL {
		t.Fatalf("%+v", got)
	}

	body, _ := json.Marshal(map[string]string{"url": srv.URL + "/index.json"})
	req := httptest.NewRequest(http.MethodPut, "/settings/marketplace-catalog", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("set: %d %s", w2.Code, w2.Body.String())
	}
	var setResp marketplaceCatalogSettingsResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &setResp)
	if setResp.EntryCount != 1 {
		t.Fatalf("%+v", setResp)
	}
	persisted := db.NewAppSettingsRepository(database).GetOrDefault(hub.SettingKey, "")
	if persisted != srv.URL+"/index.json" {
		t.Fatalf("persisted=%q", persisted)
	}

	// Refresh works
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, httptest.NewRequest(http.MethodPost, "/plugins/marketplace/refresh", nil))
	if w3.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", w3.Code, w3.Body.String())
	}

	// Provider.Get still serves
	cat, err := provider.Get(context.Background())
	if err != nil || len(cat.Entries) != 1 {
		t.Fatalf("%v %+v", err, cat)
	}
}

func TestMarketplaceCatalogSettings_UnavailableWithoutProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_ = database.Migrate()

	apiSrv := NewMarketplaceCatalogServer(nil, db.NewAppSettingsRepository(database))
	r := gin.New()
	r.GET("/settings/marketplace-catalog", apiSrv.GetMarketplaceCatalogSettings)
	r.PUT("/settings/marketplace-catalog", apiSrv.SetMarketplaceCatalogSettings)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings/marketplace-catalog", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	var got marketplaceCatalogSettingsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Configured {
		t.Fatal("expected configured=false")
	}

	body, _ := json.Marshal(map[string]string{"url": "https://hub.example/index.json"})
	req := httptest.NewRequest(http.MethodPut, "/settings/marketplace-catalog", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w2.Code)
	}
}
