package wasmhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/permissions"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/tools"
)

// CallConfig is the per-call security context passed into every WASM
// tool invocation. The host imports consult it to apply the capability
// gates ADR 0002 §3 promises:
//
//  1. Plugin manifest declared this capability? (load-time intent)
//  2. User's PermissionPolicy allows it for this call? (runtime intent)
//  3. Per-capability constraint satisfied?
//     - network.outgoing → URL host in allowed_hosts ∩ NetworkAllowlist
//     - command.exec     → binary in allowed_binaries
//     - filesystem.*     → path inside WorkspaceRoot
//
// Failing any layer returns ErrCapabilityDenied with a diagnostic
// message that surfaces in the plugin's error response.
type CallConfig struct {
	// PluginID identifies the plugin making the call (for log + audit).
	PluginID string
	// AssistantID identifies which assistant's policy applies. Empty
	// string means "system call" — the gates default to deny in that
	// case so background plugin work can't bypass user policy.
	AssistantID string
	// ConnectionID lets per-connection PermissionPolicy overrides apply
	// (ADR 0001 §7). Optional; empty falls through to the policy's
	// non-override rules.
	ConnectionID string
	// Capabilities mirrors the plugin's manifest Capabilities list —
	// the load-time declared ceiling. host imports for capabilities
	// not in this set return ErrCapabilityDenied immediately.
	Capabilities []string
	// NetworkAllowlist mirrors the plugin's manifest NetworkAllowlist
	// (ADR 0002 §3 capability-granularity policy). Intersected with
	// the policy's allowed_hosts constraint at network call time.
	NetworkAllowlist []string
	// Policy is the assistant's PermissionPolicy. Required when
	// AssistantID is set.
	Policy *domain.PermissionPolicy
	// Engine is the shared permissions engine. Injected to avoid the
	// gate functions reaching into a global.
	Engine *permissions.Engine
	// HTTPClient is the client used by host_http_request. Tests inject
	// an httptest server's client; production uses a sane default.
	HTTPClient *http.Client
	// Secrets is the secret store host_secrets_get reads from. Optional:
	// if nil, secret lookups fail with "not_found" so a misconfigured
	// daemon can't accidentally surface plaintext from another path.
	// Production always wires the real store; tests inject the in-memory
	// fake for deterministic assertions.
	Secrets secrets.Store
	// Tools dispatches the filesystem.{read,write} and command.exec
	// host imports through the same tool implementations the runtime
	// uses. Without it, those imports report capability-passed but
	// don't perform the action — fine for the gate-only test paths,
	// broken for any real plugin that needs to read a file. nil →
	// bridge stays in capability-only mode.
	Tools *tools.Executor
	// WorkspaceRoot bounds filesystem.* host imports to a directory
	// the policy already authorised. Empty → filesystem ops refused
	// even if the gate allows the capability, since we can't be sure
	// where the plugin is allowed to read or write.
	WorkspaceRoot string
}

// ErrCapabilityDenied is the sentinel returned by every gate function
// when a host call is refused. Callers wrap with context for the
// plugin-facing error message.
var ErrCapabilityDenied = errors.New("capability denied")

// callConfigKey is the context-value key under which the loader
// threads the CallConfig from CallTool into the host import bodies.
// Unexported by design — only this package writes/reads it.
type callConfigKey struct{}

// WithCallConfig threads cfg into ctx so host imports can pick it up
// during a tool call.
func WithCallConfig(ctx context.Context, cfg *CallConfig) context.Context {
	return context.WithValue(ctx, callConfigKey{}, cfg)
}

// callConfigFromContext extracts the CallConfig threaded by
// WithCallConfig. Returns nil + an error if the context wasn't
// configured — defensive default since a missing config means the
// caller forgot to wrap (production path always wraps).
func callConfigFromContext(ctx context.Context) (*CallConfig, error) {
	cfg, ok := ctx.Value(callConfigKey{}).(*CallConfig)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("wasmhost: no CallConfig in context (host import called outside CallTool)")
	}
	return cfg, nil
}

// gate is the core capability-check pipeline run by every host import.
// Returns the matching PermissionRule on success (so callers can read
// constraints), or an error wrapping ErrCapabilityDenied with a
// diagnostic explaining which layer failed.
func gate(cfg *CallConfig, capability string) (*domain.PermissionRule, error) {
	// Layer 1: manifest declared the capability.
	if !contains(cfg.Capabilities, capability) {
		return nil, fmt.Errorf("%w: plugin %q did not declare capability %q in its manifest",
			ErrCapabilityDenied, cfg.PluginID, capability)
	}
	// Layer 2: assistant must be present + policy must allow.
	if cfg.AssistantID == "" || cfg.Policy == nil || cfg.Engine == nil {
		return nil, fmt.Errorf("%w: no assistant policy threaded — system-context calls are not allowed",
			ErrCapabilityDenied)
	}
	rule := cfg.Engine.MatchingOverrideOrRule(*cfg.Policy, capability, cfg.ConnectionID)
	if rule == nil {
		return nil, fmt.Errorf("%w: no policy rule matches capability %q for assistant %q",
			ErrCapabilityDenied, capability, cfg.AssistantID)
	}
	if rule.Mode == domain.PermissionDeny {
		return nil, fmt.Errorf("%w: policy denies capability %q for assistant %q",
			ErrCapabilityDenied, capability, cfg.AssistantID)
	}
	if rule.Mode == domain.PermissionConfirm {
		// Confirm mode requires the runtime's approval flow — the WASM
		// host doesn't yet wire that path, so confirm-gated calls are
		// rejected in v1. lifecycle-07 will add approval-bridging.
		return nil, fmt.Errorf("%w: capability %q requires approval (confirm mode); WASM approval bridge lands in lifecycle-07",
			ErrCapabilityDenied, capability)
	}
	return rule, nil
}

// gateNetwork is the host_http_request specialization. Layer-3 check:
// the request URL's host must be in the intersection of the manifest's
// NetworkAllowlist and the matching rule's allowed_hosts constraint.
//
// Empty NetworkAllowlist on the manifest is interpreted as "no fixed
// hosts declared" — the user's policy allowed_hosts becomes the only
// allowlist. This matches the email-plugin extension hook documented
// in ADR 0002 §3 (per-Connection imap/smtp hosts get supplied at
// runtime instead of via the static manifest).
func gateNetwork(cfg *CallConfig, requestURL string) error {
	rule, err := gate(cfg, "network.outgoing")
	if err != nil {
		return err
	}
	parsed, perr := url.Parse(requestURL)
	if perr != nil || parsed.Host == "" {
		return fmt.Errorf("%w: cannot parse host from %q", ErrCapabilityDenied, requestURL)
	}
	host := stripPort(parsed.Host)
	manifestHosts := cfg.NetworkAllowlist
	policyHosts := stringSliceFromConstraint(rule.Constraints, "allowed_hosts")
	if !hostAllowed(host, manifestHosts, policyHosts) {
		return fmt.Errorf("%w: host %q not in plugin %q allowlist (manifest=%v policy=%v)",
			ErrCapabilityDenied, host, cfg.PluginID, manifestHosts, policyHosts)
	}
	return nil
}

// gateCommand is the host_command_exec specialization. Layer-3 check:
// the binary basename (after argv[0]) must be in the policy rule's
// allowed_binaries constraint.
func gateCommand(cfg *CallConfig, binary string) error {
	_, err := gateCommandRule(cfg, binary)
	return err
}

// gateCommandRule returns the resolved rule alongside the gate decision
// so callers (the host_command_exec import) can read the rule's
// constraints (allowed_binaries, timeout, env) without re-resolving.
func gateCommandRule(cfg *CallConfig, binary string) (*domain.PermissionRule, error) {
	rule, err := gate(cfg, "command.exec")
	if err != nil {
		return nil, err
	}
	allowed := stringSliceFromConstraint(rule.Constraints, "allowed_binaries")
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: command.exec requires non-empty allowed_binaries constraint", ErrCapabilityDenied)
	}
	if !contains(allowed, binary) {
		return nil, fmt.Errorf("%w: binary %q not in allowed_binaries %v", ErrCapabilityDenied, binary, allowed)
	}
	return rule, nil
}

// hostAllowed implements the host-matching policy described in ADR 0002 §3.
// A host is allowed when it matches any pattern in BOTH the manifest
// allowlist (intent) AND the policy allowlist (consent). Either-side
// empty means "unrestricted on that side" — empty manifest defers
// fully to the policy; empty policy defers fully to the manifest. Both
// empty means "deny" (no allowlist, no permission).
func hostAllowed(host string, manifestHosts, policyHosts []string) bool {
	if len(manifestHosts) == 0 && len(policyHosts) == 0 {
		return false
	}
	if len(manifestHosts) > 0 && !matchesAnyHost(host, manifestHosts) {
		return false
	}
	if len(policyHosts) > 0 && !matchesAnyHost(host, policyHosts) {
		return false
	}
	return true
}

// matchesAnyHost is the wildcard matcher promised by ADR 0002 §3. Same
// leading-dot anchor as the capability matcher in
// internal/permissions/engine.go::matchWildcard.
//
//	exact          → string equality
//	"*.slack.com"  → suffix match with leading dot anchor
//	"*"            → any host (escape hatch; rarely declared)
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

// stripPort strips the optional :port suffix from a Host header so
// the comparison sees a bare hostname.
func stripPort(host string) string {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}

// stringSliceFromConstraint extracts a []string constraint regardless
// of whether it was deserialized as []string or []interface{} (the
// latter is what JSON unmarshaling produces by default).
func stringSliceFromConstraint(constraints map[string]interface{}, key string) []string {
	v, ok := constraints[key]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []string:
		return x
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func contains(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

// performHTTPRequest is the post-gate worker for host_http_request.
// Bounded body read (5 MiB) so a malicious endpoint can't fill memory.
// Default timeout 30s when the call config doesn't supply a client.
func performHTTPRequest(ctx context.Context, cfg *CallConfig, method, url string, body []byte) (statusCode int32, respBody []byte, err error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	const maxBytes = 5 * 1024 * 1024
	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return int32(resp.StatusCode), nil, err //nolint:gosec // G115: HTTP status code is in [100,599], fits int32
	}
	return int32(resp.StatusCode), respBody, nil //nolint:gosec // G115: HTTP status code is in [100,599], fits int32
}
