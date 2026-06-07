package wasmhost

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/tetratelabs/wazero/api"
	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/secrets"
)

// hostHTTPRequest is the gated network egress point for marketplace
// WASM plugins. Signature: read URL + method + body from plugin memory,
// gate against (manifest network.outgoing) ∩ (policy network.outgoing
// allowed_hosts) ∩ (manifest NetworkAllowlist), perform the request,
// allocate a response buffer in plugin memory, return packed (ptr, len)
// to JSON {status, body, error}.
func hostHTTPRequest(
	ctx context.Context, mod api.Module,
	methodPtr, methodLen, urlPtr, urlLen, bodyPtr, bodyLen uint32,
) uint64 {
	method := readString(mod, methodPtr, methodLen)
	requestURL := readString(mod, urlPtr, urlLen)
	body := readBytes(mod, bodyPtr, bodyLen)

	cfg, err := callConfigFromContext(ctx)
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	if err := gateNetwork(cfg, requestURL); err != nil {
		log.Printf("[wasmhost] %s: host_http_request denied: %v", cfg.PluginID, err)
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	status, respBody, err := performHTTPRequest(ctx, cfg, method, requestURL, body)
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	return writeJSONResponse(ctx, mod, map[string]any{
		"status": status,
		"body":   string(respBody),
	})
}

// hostFilesystemRead reads a file from inside the workspace bound by
// the call config. Two layers of defence: the permissions gate
// (filesystem.read capability + matching rule) plus the tool itself,
// which performs the path-resolves-inside-root check the runtime
// already trusts. cfg.Tools missing → capability-only mode (legacy
// gate-test path).
func hostFilesystemRead(ctx context.Context, mod api.Module, pathPtr, pathLen uint32) uint64 {
	path := readString(mod, pathPtr, pathLen)
	cfg, err := callConfigFromContext(ctx)
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	rule, err := gate(cfg, "filesystem.read")
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	root := workspaceRootFor(cfg, rule)
	if root == "" {
		return writeJSONResponse(ctx, mod, map[string]any{"error": "filesystem.read: workspace_root not set"})
	}
	if cfg.Tools == nil {
		return writeJSONResponse(ctx, mod, map[string]any{
			"_capability_check": "passed",
			"error":             "filesystem.read: tools executor not wired",
		})
	}
	res := cfg.Tools.Execute(ctx, "filesystem.read", map[string]interface{}{
		"path":           path,
		"workspace_root": root,
	})
	if !res.Success {
		return writeJSONResponse(ctx, mod, map[string]any{"error": res.Error})
	}
	return writeJSONResponse(ctx, mod, map[string]any{"output": res.Output})
}

// hostFilesystemWrite mirrors hostFilesystemRead. Same defence layering.
func hostFilesystemWrite(ctx context.Context, mod api.Module, pathPtr, pathLen, contentPtr, contentLen uint32) uint64 {
	path := readString(mod, pathPtr, pathLen)
	content := readString(mod, contentPtr, contentLen)
	cfg, err := callConfigFromContext(ctx)
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	rule, err := gate(cfg, "filesystem.write")
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	root := workspaceRootFor(cfg, rule)
	if root == "" {
		return writeJSONResponse(ctx, mod, map[string]any{"error": "filesystem.write: workspace_root not set"})
	}
	if cfg.Tools == nil {
		return writeJSONResponse(ctx, mod, map[string]any{
			"_capability_check": "passed",
			"error":             "filesystem.write: tools executor not wired",
		})
	}
	res := cfg.Tools.Execute(ctx, "filesystem.write", map[string]interface{}{
		"path":           path,
		"content":        content,
		"workspace_root": root,
	})
	if !res.Success {
		return writeJSONResponse(ctx, mod, map[string]any{"error": res.Error})
	}
	return writeJSONResponse(ctx, mod, map[string]any{"output": res.Output})
}

// hostCommandExec gates command.exec with allowed_binaries constraint.
// Layer-3 check: argv[0]'s basename must be in the policy's
// allowed_binaries constraint (lifted directly from the existing
// command.exec tool's constraint shape). When cfg.Tools is wired the
// resolved argv is forwarded to the same command.exec tool the runtime
// uses, so sandbox semantics stay consistent across paths.
func hostCommandExec(ctx context.Context, mod api.Module, argvPtr, argvLen uint32) uint64 {
	argvJSON := readBytes(mod, argvPtr, argvLen)
	var argv []string
	if err := json.Unmarshal(argvJSON, &argv); err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": "host_command_exec: argv must be JSON []string"})
	}
	if len(argv) == 0 {
		return writeJSONResponse(ctx, mod, map[string]any{"error": "host_command_exec: argv empty"})
	}
	cfg, err := callConfigFromContext(ctx)
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	rule, err := gateCommandRule(cfg, argv[0])
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	if cfg.Tools == nil {
		return writeJSONResponse(ctx, mod, map[string]any{
			"_capability_check": "passed",
			"error":             "command.exec: tools executor not wired",
		})
	}
	root := workspaceRootFor(cfg, rule)
	cmdInput := map[string]interface{}{
		"command": shellQuoteArgv(argv),
	}
	if root != "" {
		cmdInput["workspace_root"] = root
	}
	if rule != nil {
		for k, v := range rule.Constraints {
			if k == "workspace_root" || k == "command" {
				continue
			}
			cmdInput[k] = v
		}
	}
	res := cfg.Tools.Execute(ctx, "command.exec", cmdInput)
	if !res.Success {
		return writeJSONResponse(ctx, mod, map[string]any{"error": res.Error})
	}
	return writeJSONResponse(ctx, mod, map[string]any{"output": res.Output})
}

// workspaceRootFor picks the root the filesystem tools should sandbox
// against. CallConfig.WorkspaceRoot wins when set (caller knows what
// it's doing); otherwise we fall back to the rule's workspace_root
// constraint, which mirrors how the runtime threads the value.
func workspaceRootFor(cfg *CallConfig, rule *domain.PermissionRule) string {
	if cfg != nil && cfg.WorkspaceRoot != "" {
		return cfg.WorkspaceRoot
	}
	if rule != nil {
		if root, ok := rule.Constraints["workspace_root"].(string); ok {
			return root
		}
	}
	return ""
}

// shellQuoteArgv joins argv into a single command string the
// command.exec tool can parse. The tool refuses shell metacharacters,
// so we don't need to escape — argv that survives the gate is already
// constrained to safe binaries with simple arguments. Spaces inside an
// arg are quoted so multi-word values don't split into separate args.
func shellQuoteArgv(argv []string) string {
	out := ""
	for i, a := range argv {
		if i > 0 {
			out += " "
		}
		if needsQuoting(a) {
			out += "\"" + a + "\""
			continue
		}
		out += a
	}
	return out
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == ' ' || r == '\t' {
			return true
		}
	}
	return false
}

// hostSecretsGet gates secrets access. There's no per-secret constraint
// — the gate is binary, intentionally so per ADR 0002 §3 (plugins
// shouldn't request per-secret access; if they need many secrets they
// should be redesigned).
func hostSecretsGet(ctx context.Context, mod api.Module, keyPtr, keyLen uint32) uint64 {
	key := readString(mod, keyPtr, keyLen)
	cfg, err := callConfigFromContext(ctx)
	if err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	if _, err := gate(cfg, "secrets.read"); err != nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": err.Error()})
	}
	// Secret namespace is partitioned per plugin so a malicious or
	// confused plugin can't read another plugin's bot tokens. Key the
	// lookup as plugins/<plugin_id>/<requested_key>.
	scopedKey := "plugins/" + cfg.PluginID + "/" + key
	if cfg.Secrets == nil {
		return writeJSONResponse(ctx, mod, map[string]any{"error": "not_found"})
	}
	value, lookupErr := cfg.Secrets.Get(scopedKey)
	if lookupErr != nil {
		if errors.Is(lookupErr, secrets.ErrNotFound) {
			return writeJSONResponse(ctx, mod, map[string]any{"error": "not_found"})
		}
		return writeJSONResponse(ctx, mod, map[string]any{"error": lookupErr.Error()})
	}
	return writeJSONResponse(ctx, mod, map[string]any{"value": value})
}

// --- shared memory + JSON helpers ---

// readString reads a UTF-8 string out of plugin memory at (ptr, len).
// Returns "" on bounds violation rather than panicking.
func readString(mod api.Module, ptr, length uint32) string {
	body := readBytes(mod, ptr, length)
	return string(body)
}

// readBytes reads (ptr, len) bytes from the plugin's linear memory.
// Returns nil on bounds violation. Callers downstream of read are
// responsible for input validation; this helper is purely about the
// memory boundary.
func readBytes(mod api.Module, ptr, length uint32) []byte {
	if length == 0 {
		return nil
	}
	body, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil
	}
	// Wazero returns a view into the underlying memory which can be
	// invalidated by subsequent writes — copy so callers own the data.
	out := make([]byte, len(body))
	copy(out, body)
	return out
}

// writeJSONResponse serializes resp, asks the plugin to alloc a buffer
// of the right size, copies the bytes in, and returns the packed
// (ptr<<32 | len) i64 host imports must return. The plugin is
// responsible for calling dealloc once it has read the bytes.
func writeJSONResponse(ctx context.Context, mod api.Module, resp map[string]any) uint64 {
	body, err := json.Marshal(resp)
	if err != nil {
		return 0
	}
	allocFn := mod.ExportedFunction("alloc")
	if allocFn == nil {
		log.Printf("[wasmhost] writeJSONResponse: plugin missing alloc export, response dropped")
		return 0
	}
	results, err := allocFn.Call(ctx, uint64(len(body)))
	if err != nil || len(results) != 1 {
		log.Printf("[wasmhost] writeJSONResponse: alloc(%d) failed: %v", len(body), err)
		return 0
	}
	ptr := uint32(results[0])
	if !mod.Memory().Write(ptr, body) {
		log.Printf("[wasmhost] writeJSONResponse: write(%d bytes @ %d) out of bounds", len(body), ptr)
		return 0
	}
	return PackPtrLen(ptr, uint32(len(body)))
}
