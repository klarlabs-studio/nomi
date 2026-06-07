package wasmhost

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/permissions"
)

// echoWASM is the compiled echo plugin from examples/wasm-plugin-echo/.
// Rebuild with `make wasm-echo` after editing the source. Embedded here
// so the tests don't depend on TinyGo being installed at run time —
// the .wasm bytes are committed to testdata/.
//
//go:embed testdata/echo.wasm
var echoWASM []byte

// TestEchoPlugin_FullRoundTrip is the spike's whole reason for existing:
// load a WASM plugin, call its manifest export, call its tool_execute
// export with a JSON payload, decode the JSON response. If this passes,
// the WASM ABI defined in wasmhost.go works end-to-end and lifecycle-04
// (gated host imports) can layer on top with confidence.
func TestEchoPlugin_FullRoundTrip(t *testing.T) {
	ctx := context.Background()
	loader := NewLoader(ctx)
	defer loader.Close(ctx)

	mod, err := loader.Load(ctx, "com.example.echo", echoWASM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer mod.Close(ctx)

	// Manifest round-trip: WASM emits JSON, host reads + parses it.
	manifest, err := mod.Manifest(ctx)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if got, _ := manifest["id"].(string); got != "com.example.echo" {
		t.Fatalf("manifest id = %v, want com.example.echo", manifest["id"])
	}
	if got, _ := manifest["name"].(string); got != "Echo Plugin" {
		t.Fatalf("manifest name = %v", manifest["name"])
	}

	// Tool round-trip: host writes JSON into plugin memory via alloc,
	// plugin reads it, returns its own JSON via packed (ptr, len), host
	// reads the response back. Validates the entire ABI in one call.
	resp, err := mod.CallTool(ctx, "echo.echo", map[string]any{
		"message": "ping",
		"count":   float64(42), // JSON numbers decode as float64
	}, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %+v", resp)
	}
	echoed, ok := result["echoed"].(map[string]any)
	if !ok {
		t.Fatalf("response missing echoed: %+v", result)
	}
	if got := echoed["message"]; got != "ping" {
		t.Fatalf("echoed message = %v, want ping", got)
	}
	if got := echoed["count"]; got != float64(42) {
		t.Fatalf("echoed count = %v, want 42", got)
	}
	if tool := result["tool"]; tool != "echo.echo" {
		t.Fatalf("echoed tool name = %v, want echo.echo", tool)
	}
}

// TestEchoPlugin_LoadRejectsMissingExports validates the fail-fast guard
// in Load that catches plugins missing required ABI functions. Build
// a minimal WASM module that has none of our required exports — Load
// should refuse it before any user code touches it.
func TestEchoPlugin_LoadRejectsMissingExports(t *testing.T) {
	ctx := context.Background()
	loader := NewLoader(ctx)
	defer loader.Close(ctx)

	// Smallest valid WASM module (magic + version, no sections).
	emptyModule := []byte{
		0x00, 0x61, 0x73, 0x6d, // \0asm
		0x01, 0x00, 0x00, 0x00, // version 1
	}
	_, err := loader.Load(ctx, "com.example.empty", emptyModule)
	if err == nil {
		t.Fatal("expected Load to reject module missing required exports")
	}
}

// TestEchoFetch_AllowedHostRoundTrips exercises host_http_request from
// inside the WASM module: the plugin's manifest declares
// network.outgoing + a NetworkAllowlist; the policy allows
// network.outgoing with a matching allowed_hosts constraint; the host
// proxies the request to a local httptest server. The plugin reads the
// response body back, validating the entire gated-import round-trip.
func TestEchoFetch_AllowedHostRoundTrips(t *testing.T) {
	ctx := context.Background()
	loader := NewLoader(ctx)
	defer loader.Close(ctx)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	}))
	defer srv.Close()
	// Strip the "http://" prefix to derive the host the gate checks.
	host := strings.TrimPrefix(srv.URL, "http://")
	host = strings.SplitN(host, ":", 2)[0] // drop the port

	mod, err := loader.Load(ctx, "com.example.echo", echoWASM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer mod.Close(ctx)

	cfg := &CallConfig{
		PluginID:         "com.example.echo",
		AssistantID:      "asst-1",
		Capabilities:     []string{"network.outgoing"},
		NetworkAllowlist: []string{host}, // matches the test server's host
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "network.outgoing", Mode: domain.PermissionAllow,
					Constraints: map[string]interface{}{"allowed_hosts": []string{host}}},
			},
		},
		Engine:     permissions.NewEngine(),
		HTTPClient: srv.Client(),
	}
	resp, err := mod.CallTool(ctx, "echo.fetch", map[string]any{
		"method": "GET",
		"url":    srv.URL,
	}, cfg)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	result, _ := resp["result"].(map[string]any)
	raw, _ := result["raw_response"].(string)

	// raw is the JSON the host wrote back: {"status":200,"body":"..."}
	var wrap struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, raw)
	}
	if wrap.Error != "" {
		t.Fatalf("expected success, got error %q", wrap.Error)
	}
	if wrap.Status != 200 || !strings.Contains(wrap.Body, "world") {
		t.Fatalf("unexpected response: status=%d body=%q", wrap.Status, wrap.Body)
	}
}

// TestEchoFetch_DeniedByPolicy verifies the per-call policy gate fires
// when network.outgoing is set to deny — even though the manifest
// declares the capability and the host is in the allowlist, the
// policy override wins.
func TestEchoFetch_DeniedByPolicy(t *testing.T) {
	ctx := context.Background()
	loader := NewLoader(ctx)
	defer loader.Close(ctx)

	mod, err := loader.Load(ctx, "com.example.echo", echoWASM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer mod.Close(ctx)

	cfg := &CallConfig{
		PluginID:         "com.example.echo",
		AssistantID:      "asst-1",
		Capabilities:     []string{"network.outgoing"},
		NetworkAllowlist: []string{"*"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "network.outgoing", Mode: domain.PermissionDeny},
			},
		},
		Engine: permissions.NewEngine(),
	}
	resp, err := mod.CallTool(ctx, "echo.fetch", map[string]any{
		"url": "http://does-not-matter.invalid/x",
	}, cfg)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	result, _ := resp["result"].(map[string]any)
	raw, _ := result["raw_response"].(string)
	if !strings.Contains(raw, "denied") {
		t.Fatalf("expected denial in response, got %q", raw)
	}
}

// TestEchoFetch_DeniedByHostAllowlist verifies the host-allowlist
// intersection: policy allows network.outgoing but only for example.com;
// plugin tries attacker.com → denied.
func TestEchoFetch_DeniedByHostAllowlist(t *testing.T) {
	ctx := context.Background()
	loader := NewLoader(ctx)
	defer loader.Close(ctx)

	mod, err := loader.Load(ctx, "com.example.echo", echoWASM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer mod.Close(ctx)

	cfg := &CallConfig{
		PluginID:         "com.example.echo",
		AssistantID:      "asst-1",
		Capabilities:     []string{"network.outgoing"},
		NetworkAllowlist: []string{"example.com"},
		Policy: &domain.PermissionPolicy{
			Rules: []domain.PermissionRule{
				{Capability: "network.outgoing", Mode: domain.PermissionAllow,
					Constraints: map[string]interface{}{"allowed_hosts": []string{"example.com"}}},
			},
		},
		Engine: permissions.NewEngine(),
	}
	resp, err := mod.CallTool(ctx, "echo.fetch", map[string]any{
		"url": "http://attacker.com/exfil",
	}, cfg)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	result, _ := resp["result"].(map[string]any)
	raw, _ := result["raw_response"].(string)
	if !strings.Contains(raw, "not in plugin") {
		t.Fatalf("expected host-allowlist denial, got %q", raw)
	}
}

// TestPackPtrLen_RoundTrip pins the i64 packing convention used across
// the ABI — both sides (host + plugin) must agree exactly or every
// memory read is garbage. Worth a unit test because every other
// function depends on it.
func TestPackPtrLen_RoundTrip(t *testing.T) {
	cases := []struct {
		ptr, length uint32
	}{
		{0, 0},
		{1, 1},
		{0xFFFFFFFF, 0},
		{0, 0xFFFFFFFF},
		{0xCAFEBABE, 0xDEADBEEF},
	}
	for _, c := range cases {
		packed := PackPtrLen(c.ptr, c.length)
		gotPtr, gotLen := unpackPtrLen(packed)
		if gotPtr != c.ptr || gotLen != c.length {
			t.Fatalf("round-trip (%x, %x) -> %x -> (%x, %x)", c.ptr, c.length, packed, gotPtr, gotLen)
		}
	}
}
