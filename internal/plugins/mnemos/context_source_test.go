package mnemos

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/felixgeelhaar/nomi/internal/plugins"
)

// helper that returns an httptest.Server speaking just enough of the
// Mnemos /v1/context shape to validate the context source's wiring.
func newStubMnemosServer(t *testing.T, contextBody string) (*httptest.Server, *stubMnemosCapture) {
	t.Helper()
	cap := &stubMnemosCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/context") {
			http.Error(w, "stub: unsupported path "+r.URL.Path, http.StatusNotFound)
			return
		}
		cap.lastRunID = r.URL.Query().Get("run_id")
		cap.lastQuery = r.URL.Query().Get("query")
		cap.lastMaxTokens = r.URL.Query().Get("max_tokens")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"run_id":  cap.lastRunID,
			"context": contextBody,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type stubMnemosCapture struct {
	lastRunID     string
	lastQuery     string
	lastMaxTokens string
}

func TestContextSource_EmptyRunID_ReturnsEmpty(t *testing.T) {
	p := New(&fakeSecrets{})
	cfg, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{{ID: "c1", BaseURL: "http://unreachable"}},
	})
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	sources := p.ContextSources()
	if len(sources) != 1 {
		t.Fatalf("want 1 source, got %d", len(sources))
	}
	// No RunID supplied → returns empty without hitting the network.
	out, err := sources[0].Query(context.Background(), plugins.ContextQueryRequest{Goal: "anything"})
	if err != nil {
		t.Errorf("empty-runID Query should not error: %v", err)
	}
	if out != "" {
		t.Errorf("empty-runID Query should return empty string, got %q", out)
	}
}

func TestContextSource_WithRunID_HitsUpstream(t *testing.T) {
	srv, capture := newStubMnemosServer(t, "## Recent claims\n- foo\n- bar")

	p := New(&fakeSecrets{})
	cfg, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{{ID: "c1", BaseURL: srv.URL}},
	})
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	sources := p.ContextSources()
	if len(sources) != 1 {
		t.Fatalf("want 1 source, got %d", len(sources))
	}

	out, err := sources[0].Query(context.Background(), plugins.ContextQueryRequest{
		RunID:     "run-42",
		Goal:      "rate limiting",
		MaxTokens: 1500,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(out, "Recent claims") {
		t.Errorf("expected upstream body, got %q", out)
	}

	// Verify the request carried our parameters.
	if capture.lastRunID != "run-42" {
		t.Errorf("run_id query param = %q, want run-42", capture.lastRunID)
	}
	if capture.lastQuery != "rate limiting" {
		t.Errorf("query param = %q, want 'rate limiting'", capture.lastQuery)
	}
	if capture.lastMaxTokens != "1500" {
		t.Errorf("max_tokens param = %q, want 1500", capture.lastMaxTokens)
	}
}

func TestContextSource_MaxTokensFallback(t *testing.T) {
	srv, capture := newStubMnemosServer(t, "ok")
	p := New(&fakeSecrets{})
	cfg, _ := json.Marshal(configureInput{
		Connections: []connectionConfig{{ID: "c1", BaseURL: srv.URL}},
	})
	if err := p.Configure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// MaxTokens omitted (zero) → source uses its internal default.
	_, err := p.ContextSources()[0].Query(context.Background(), plugins.ContextQueryRequest{
		RunID: "run-99",
		Goal:  "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if capture.lastMaxTokens != "2000" {
		t.Errorf("default max_tokens = %q, want 2000", capture.lastMaxTokens)
	}
}

// drainBody is a tiny helper for tests that need to assert request
// bodies. Currently unused; kept for future POST-shape tests.
func drainBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

var _ = drainBody // suppress unused warning while only GET endpoints are tested
