package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"go.klarlabs.de/nomi/internal/storage/db"
)

func TestImportRecipe_RoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(); err != nil {
		t.Fatal(err)
	}

	srv := NewRecipeServer(db.NewRecipeRepository(database), db.NewAssistantRepository(database))
	r := gin.New()
	r.POST("/recipes/import", srv.ImportRecipe)
	r.GET("/recipes", srv.ListRecipes)

	yaml := `
schema_version: 1
id: test.imported.recipe
name: Imported Test
version: 0.1.0
author: test
description: shareable
assistant:
  name: Imported Test
  role: helper
  system_prompt: You help with tests.
`
	body, _ := json.Marshal(map[string]string{"yaml": yaml})
	req := httptest.NewRequest(http.MethodPost, "/recipes/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("import status %d: %s", w.Code, w.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/recipes", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list status %d", w2.Code)
	}
	var listed struct {
		Recipes []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"recipes"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range listed.Recipes {
		if e.ID == "test.imported.recipe" && e.Source == "imported" {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported recipe missing from list: %+v", listed.Recipes)
	}
}

func TestImportRecipe_RejectsBadYAML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := db.New(db.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_ = database.Migrate()
	srv := NewRecipeServer(db.NewRecipeRepository(database), db.NewAssistantRepository(database))
	r := gin.New()
	r.POST("/recipes/import", srv.ImportRecipe)

	body, _ := json.Marshal(map[string]string{"yaml": "not: a: recipe"})
	req := httptest.NewRequest(http.MethodPost, "/recipes/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusCreated {
		t.Fatal("expected validation error")
	}
}
