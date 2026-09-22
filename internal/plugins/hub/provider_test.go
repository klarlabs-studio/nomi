package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCachedProvider_RoundTrip(t *testing.T) {
	rootPub, rootPriv, _ := newKeysAndClient(t)
	signed := signCatalog(t, rootPriv, sampleCatalog())
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(signed)
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(rootPub, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := NewCachedProvider(client, srv.URL+"/index.json")
	ctx := context.Background()
	c1, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.Entries) == 0 {
		t.Fatal("empty catalog")
	}
	if _, err := p.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("cache miss: hits=%d", hits)
	}
	if _, err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("refresh hits=%d", hits)
	}
	st := p.Status()
	if st.EntryCount != len(c1.Entries) {
		t.Fatalf("%+v", st)
	}
}

func TestCachedProvider_SetURLClearsCache(t *testing.T) {
	rootPub, rootPriv, _ := newKeysAndClient(t)
	signed := signCatalog(t, rootPriv, sampleCatalog())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(signed)
	}))
	t.Cleanup(srv.Close)
	client, _ := NewClient(rootPub, srv.Client())
	p := NewCachedProvider(client, srv.URL)
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetURL("ftp://bad"); err == nil {
		t.Fatal("expected scheme error")
	}
	if err := p.SetURL(""); err != nil {
		t.Fatal(err)
	}
	if p.Status().EntryCount != 0 {
		t.Fatal("cache should clear")
	}
	if p.EffectiveURL() != DefaultCatalogURL {
		t.Fatalf("effective=%s", p.EffectiveURL())
	}
}

func TestValidateCatalogURL(t *testing.T) {
	if err := ValidateCatalogURL("https://hub.example/index.json"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCatalogURL("http://127.0.0.1:9/c.json"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCatalogURL("not-a-url"); err == nil {
		t.Fatal("expected error")
	}
}
