// Per-domain gating for browser navigation (browser-03). Each
// Connection declares an allowed_hosts list; browser.navigate
// refuses URLs whose host doesn't match any pattern. Once a page
// loads, subsequent interact/extract tools trust the page — the
// gate is at the edge (navigate), not at every interaction. This
// matches what users expect: "I told the assistant it can use
// example.com; clicking buttons on example.com is implicit."
//
// Empty allowed_hosts deny-all. Users who want unrestricted
// browsing set allowed_hosts: ["*"] explicitly — the explicit
// escape hatch beats a permissive default.
//
// Wildcard matching duplicates the small helper set from
// internal/plugins/wasmhost/gates.go for now; if a third caller
// shows up, promote these to internal/hostmatch as a shared util.

package browser

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// ErrHostNotAllowed is the sentinel for navigation refusals. The
// install dialog and the runs UI both branch on this so refusals
// surface as a security message rather than a generic tool failure.
var ErrHostNotAllowed = errors.New("browser: host not in allowed_hosts")

// allowedHostsFromConfig parses the connection config's
// allowed_hosts field. Returns nil for empty/missing/wrong-type so
// the caller's empty-list deny-all path triggers naturally rather
// than treating "no field" as "allow everything."
func allowedHostsFromConfig(cfg map[string]any) []string {
	switch v := cfg["allowed_hosts"].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// gateNavigate checks whether the connection allows navigating to
// rawURL. Returns nil on allow, ErrHostNotAllowed-wrapped on deny.
// A malformed URL is treated as deny — never silently allowed.
func gateNavigate(conn *domain.Connection, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%w: cannot parse host from %q", ErrHostNotAllowed, rawURL)
	}
	host := stripPort(parsed.Host)
	allowed := allowedHostsFromConfig(conn.Config)
	if len(allowed) == 0 {
		return fmt.Errorf("%w: connection has no allowed_hosts (set explicitly to enable browsing; use [\"*\"] for unrestricted)",
			ErrHostNotAllowed)
	}
	if !matchesAnyHost(host, allowed) {
		return fmt.Errorf("%w: %q not in connection allowlist %v",
			ErrHostNotAllowed, host, allowed)
	}
	return nil
}

// matchesAnyHost returns true when host matches at least one
// pattern. Patterns: exact ("api.slack.com"), wildcard subdomain
// ("*.slack.com" — leading-dot anchored, does NOT match
// "slack.com.attacker.com"), or "*" (allow-all escape hatch).
func matchesAnyHost(host string, patterns []string) bool {
	for _, p := range patterns {
		if matchHost(p, host) {
			return true
		}
	}
	return false
}

func matchHost(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := strings.TrimPrefix(pattern, "*.")
		return strings.HasSuffix(host, "."+suffix) || host == suffix
	}
	return false
}

// stripPort strips the :port suffix so the comparison sees a bare
// hostname.
func stripPort(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}
