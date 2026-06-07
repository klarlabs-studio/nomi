package browser

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

// --- pure helpers -------------------------------------------------

func TestMatchHost_Patterns(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"api.slack.com", "api.slack.com", true},
		{"api.slack.com", "files.slack.com", false},
		{"*.slack.com", "files.slack.com", true},
		{"*.slack.com", "slack.com", true},
		{"*.slack.com", "slack.com.attacker.com", false}, // leading-dot anchor
		{"*", "anything.example.org", true},
		{"github.com", "raw.github.com", false},
	}
	for _, c := range cases {
		if got := matchHost(c.pattern, c.host); got != c.want {
			t.Errorf("matchHost(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"example.com":     "example.com",
		"example.com:443": "example.com",
		"localhost:8080":  "localhost",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Fatalf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowedHostsFromConfig(t *testing.T) {
	// JSON-unmarshaled []interface{}
	got := allowedHostsFromConfig(map[string]any{"allowed_hosts": []interface{}{"a", "b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("[]interface{} parse drift: %v", got)
	}
	// Native []string
	got = allowedHostsFromConfig(map[string]any{"allowed_hosts": []string{"x"}})
	if len(got) != 1 || got[0] != "x" {
		t.Fatalf("[]string parse drift: %v", got)
	}
	// Empty / wrong-type → nil (deny-all)
	if got := allowedHostsFromConfig(map[string]any{}); got != nil {
		t.Fatalf("missing field should yield nil, got %v", got)
	}
	if got := allowedHostsFromConfig(map[string]any{"allowed_hosts": "single-string"}); got != nil {
		t.Fatalf("wrong type should yield nil, got %v", got)
	}
}

// --- gateNavigate end-to-end -------------------------------------

func TestGateNavigate_AllowsExactMatch(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{
		"allowed_hosts": []interface{}{"example.com"},
	}}
	if err := gateNavigate(conn, "https://example.com/page"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

func TestGateNavigate_AllowsWildcard(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{
		"allowed_hosts": []interface{}{"*.github.com"},
	}}
	if err := gateNavigate(conn, "https://api.github.com/user"); err != nil {
		t.Fatalf("wildcard subdomain should allow, got %v", err)
	}
}

func TestGateNavigate_DeniesNonMatching(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{
		"allowed_hosts": []interface{}{"example.com"},
	}}
	err := gateNavigate(conn, "https://attacker.invalid/")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("want ErrHostNotAllowed, got %v", err)
	}
}

func TestGateNavigate_EmptyAllowlistDeniesAll(t *testing.T) {
	// Default config (no allowed_hosts field) must deny — never
	// silently allow. Users opt in by setting the list.
	conn := &domain.Connection{Config: map[string]any{}}
	err := gateNavigate(conn, "https://example.com/")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("empty allowlist should deny, got %v", err)
	}
}

func TestGateNavigate_RejectsMalformedURL(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{
		"allowed_hosts": []interface{}{"*"},
	}}
	// No scheme = no host as far as url.Parse is concerned
	err := gateNavigate(conn, "not a url")
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("malformed URL should deny, got %v", err)
	}
}

func TestGateNavigate_StripsPort(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{
		"allowed_hosts": []interface{}{"localhost"},
	}}
	if err := gateNavigate(conn, "http://localhost:3000/"); err != nil {
		t.Fatalf("port should be stripped before match, got %v", err)
	}
}

func TestGateNavigate_StarIsExplicitEscapeHatch(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{
		"allowed_hosts": []interface{}{"*"},
	}}
	for _, u := range []string{"https://random.example/", "https://10.0.0.1/", "https://internal.corp/"} {
		if err := gateNavigate(conn, u); err != nil {
			t.Fatalf("%s under * should allow, got %v", u, err)
		}
	}
}

// --- end-to-end: tool.Execute respects the gate -----------------

func TestNavigate_DeniedByConnectionAllowlist(t *testing.T) {
	f := newFixture(t)
	f.withAllowedHosts(t, []string{"example.com"})
	_, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID,
		"url":           "https://attacker.invalid/exfil",
	})
	if err == nil {
		t.Fatal("expected gate to deny attacker.invalid")
	}
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("expected ErrHostNotAllowed, got %v", err)
	}
	// Crucially: the MCP client must NEVER have been invoked when
	// the gate fires. A leaky gate that ran the call before
	// blocking would defeat the point.
	if len(f.invoker.calls) != 0 {
		t.Fatalf("MCP invoker should not be called when gate denies, got calls: %v", f.invoker.calls)
	}
}

func TestNavigate_AllowedThenInteractWithoutGate(t *testing.T) {
	// The deliberate model: navigate gates by URL; subsequent
	// interact tools trust the loaded page. Confirm a click after a
	// gated navigate is NOT subject to the host check.
	f := newFixture(t)
	f.withAllowedHosts(t, []string{"example.com"})
	if _, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID, "url": "https://example.com/login",
	}); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if _, err := f.tool("browser.click")(map[string]interface{}{
		"connection_id": f.connID, "selector": "#submit",
	}); err != nil {
		t.Fatalf("click after allowed navigate should succeed, got %v", err)
	}
}

// Sanity that the existing happy-path navigate test (uses the
// permissive "*" allowlist from newFixture) still works after the
// gate landed.
func TestNavigate_StarAllowlistStillWorks(t *testing.T) {
	f := newFixture(t)
	if _, err := f.tool("browser.navigate")(map[string]interface{}{
		"connection_id": f.connID,
		"url":           "https://anything.example/",
	}); err != nil {
		t.Fatalf("* allowlist should allow anything: %v", err)
	}
}

// _ keeps the context import alive when no test in this file uses it.
var _ = context.Background
