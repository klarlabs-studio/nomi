package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	gh "go.klarlabs.de/nomi/internal/integrations/github"
)

// stubServer wires an httptest.Server that mints installation tokens
// (so AuthClient is satisfied) and serves whatever response the test
// queues for a given path. Returned with a cleanup func.
type stubServer struct {
	srv     *httptest.Server
	pathMap map[string]stubResponse
	calls   []string
}

type stubResponse struct {
	status int
	body   string
	header http.Header
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{pathMap: map[string]stubResponse{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls = append(s.calls, r.Method+" "+r.URL.RequestURI())
		// Installation token endpoint always returns a valid token
		// expiring an hour out.
		if strings.Contains(r.URL.Path, "/access_tokens") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_stub","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key = r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		}
		// Try exact match first; fall back to method+path so tests
		// don't have to enumerate every query-string variant.
		resp, ok := s.pathMap[key]
		if !ok {
			resp, ok = s.pathMap[r.Method+" "+r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"message":"unstubbed: %s"}`, key)
			return
		}
		for k, vs := range resp.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubServer) stub(methodPath string, status int, body string) {
	s.pathMap[methodPath] = stubResponse{status: status, body: body}
}

// stubPlugin wires a Plugin whose authClientFor returns an AuthClient
// pointed at the stub server. Returns the plugin + a Connection
// pre-populated with valid-looking config.
func stubPlugin(t *testing.T, srv *stubServer) (*Plugin, *domain.Connection) {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(rsaKey),
	})
	creds, err := gh.LoadAppCredentials(42, pemBytes)
	if err != nil {
		t.Fatalf("LoadAppCredentials: %v", err)
	}
	auth := gh.NewAuthClient(creds, gh.WithAPIBase(srv.srv.URL), gh.WithHTTPClient(srv.srv.Client()))

	p := NewPlugin(nil, nil, nil)
	p.SetAuthOverride(func(_ *domain.Connection) (*gh.AuthClient, error) { return auth, nil })

	conn := &domain.Connection{
		ID:       "c1",
		PluginID: PluginID,
		Enabled:  true,
		Config: map[string]any{
			configAppID:          float64(42),
			configInstallationID: float64(555),
			configAccountLogin:   "testorg",
		},
	}
	return p, conn
}

func TestIssuesList_FiltersPRs(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/issues",
		http.StatusOK,
		`[
			{"number":1,"title":"A bug","state":"open","user":{"login":"alice"},"labels":[{"name":"bug"}]},
			{"number":2,"title":"A PR","state":"open","pull_request":{"url":"x"}},
			{"number":3,"title":"Another bug","state":"open","user":{"login":"bob"}}
		]`,
	)
	p, conn := stubPlugin(t, srv)
	out, err := p.issuesList(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
	})
	if err != nil {
		t.Fatalf("issuesList: %v", err)
	}
	issues, _ := out["issues"].([]map[string]any)
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues (PR filtered out), got %d: %+v", len(issues), issues)
	}
	if labels, _ := issues[0]["labels"].([]string); len(labels) != 1 || labels[0] != "bug" {
		t.Fatalf("expected label flattening, got %+v", issues[0])
	}
	if author, _ := issues[0]["author"].(string); author != "alice" {
		t.Fatalf("expected author=alice, got %v", issues[0]["author"])
	}
}

func TestIssuesGet_BundlesComments(t *testing.T) {
	srv := newStubServer(t)
	srv.stub("GET /repos/o/r/issues/7", http.StatusOK,
		`{"number":7,"title":"X","state":"open","body":"hi","user":{"login":"alice"}}`)
	srv.stub("GET /repos/o/r/issues/7/comments",
		http.StatusOK,
		`[{"id":1,"body":"first","user":{"login":"bob"}},{"id":2,"body":"second","user":{"login":"alice"}}]`)
	p, conn := stubPlugin(t, srv)
	out, err := p.issuesGet(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r", "issue_number": float64(7),
	})
	if err != nil {
		t.Fatalf("issuesGet: %v", err)
	}
	comments, _ := out["comments"].([]map[string]any)
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
}

func TestIssuesCreate_PassesPayload(t *testing.T) {
	srv := newStubServer(t)
	var seenBody string
	mu := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Manually intercept just this one path so we can verify
		// the request body. Re-emits the stubResponse for everything
		// else.
		if r.Method == "POST" && r.URL.Path == "/repos/o/r/issues" {
			b, _ := readAll(r.Body)
			seenBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":99,"title":"Created","state":"open"}`))
			return
		}
		// Token endpoint
		if strings.Contains(r.URL.Path, "/access_tokens") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_stub","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv.srv.Config.Handler = mu

	p, conn := stubPlugin(t, srv)
	_, err := p.issuesCreate(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"title": "Created", "labels": "bug,wontfix", "assignees": []string{"alice"},
	})
	if err != nil {
		t.Fatalf("issuesCreate: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(seenBody), &sent); err != nil {
		t.Fatalf("decode sent body %q: %v", seenBody, err)
	}
	if sent["title"] != "Created" {
		t.Fatalf("title = %v", sent["title"])
	}
	labels, _ := sent["labels"].([]any)
	if len(labels) != 2 {
		t.Fatalf("labels = %v", sent["labels"])
	}
}

func TestIssuesComment_RequiresBody(t *testing.T) {
	srv := newStubServer(t)
	p, conn := stubPlugin(t, srv)
	_, err := p.issuesComment(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
		"issue_number": float64(1),
		// no body
	})
	if err == nil {
		t.Fatal("want error when body missing")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Fatalf("error should mention body, got %v", err)
	}
}

func TestAssertRepoAllowed_EmptyAllowlistPermissive(t *testing.T) {
	p := &Plugin{}
	conn := &domain.Connection{Config: map[string]any{}}
	if err := p.assertRepoAllowed(conn, "o", "r"); err != nil {
		t.Fatalf("empty allowlist should permit any repo: %v", err)
	}
}

func TestAssertRepoAllowed_RestrictsToList(t *testing.T) {
	p := &Plugin{}
	conn := &domain.Connection{Config: map[string]any{
		configRepoAllowlist: "Acme/Widgets, acme/gadgets",
	}}
	if err := p.assertRepoAllowed(conn, "acme", "widgets"); err != nil {
		t.Fatalf("case-insensitive match should pass: %v", err)
	}
	if err := p.assertRepoAllowed(conn, "acme", "secrets"); err == nil {
		t.Fatal("non-allowlisted repo should error")
	}
}

func TestRateLimitError_TypedSurface(t *testing.T) {
	srv := newStubServer(t)
	srv.pathMap["GET /repos/o/r/issues"] = stubResponse{
		status: http.StatusForbidden,
		body:   `{"message":"API rate limit exceeded"}`,
		header: http.Header{
			"X-RateLimit-Remaining": []string{"0"},
			"X-RateLimit-Reset":     []string{fmt.Sprintf("%d", time.Now().Add(15*time.Minute).Unix())},
		},
	}
	p, conn := stubPlugin(t, srv)
	_, err := p.issuesList(context.Background(), conn, map[string]any{
		"connection_id": "c1", "owner": "o", "repo": "r",
	})
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	var rl *gh.RateLimitError
	if !errorAs(err, &rl) {
		t.Fatalf("expected *gh.RateLimitError, got %T: %v", err, err)
	}
	if rl.Remaining != 0 {
		t.Fatalf("Remaining = %d", rl.Remaining)
	}
}

// errorAs is a tiny errors.As wrapper. The dependency on errors is
// minor but keeping the test imports tight has its own value.
func errorAs(err error, target any) bool {
	for {
		if err == nil {
			return false
		}
		if x, ok := err.(*gh.RateLimitError); ok {
			if t, ok := target.(**gh.RateLimitError); ok {
				*t = x
				return true
			}
		}
		// unwrap not needed; integration test wraps simply.
		return false
	}
}

// readAll reads an io.Reader fully without dragging in io.ReadAll
// resolution at test time.
func readAll(r interface{ Read(p []byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	chunk := make([]byte, 1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
