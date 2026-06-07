// Echo plugin — the smallest WASM plugin that exercises every part of
// the Nomi plugin ABI (ADR 0002). Compiled with TinyGo:
//
//	tinygo build -o echo.wasm -target=wasi -no-debug ./examples/wasm-plugin-echo/
//
// The compiled .wasm lives at internal/plugins/wasmhost/testdata/echo.wasm
// and is loaded by the wasmhost spike test. Rebuild with `make wasm-echo`
// when changing this file.
//
// The build constraint excludes this file from the standard `go test
// ./...` discovery — it uses //go:wasmimport which only TinyGo
// understands. CI runs the compiled .wasm via the wasmhost tests; the
// source compiles via TinyGo via `make wasm-echo`.

//go:build tinygo

package main

import (
	"encoding/json"
	"unsafe"
)

// We need a main for TinyGo's WASI target but it's never run — the
// host calls our exports directly.
func main() {}

// host_http_request is the gated egress import. //go:wasmimport
// generates an "env"."host_http_request" import entry in the WASM
// module so wazero can link the host-provided implementation in
// wasmhost/imports.go at instantiate time.
//
//go:wasmimport env host_http_request
func hostHTTPRequest(methodPtr, methodLen, urlPtr, urlLen, bodyPtr, bodyLen uint32) uint64

// alloc is the host-allocated buffer entry point. The host calls this
// to reserve N bytes inside our linear memory, then writes its payload
// in. We use Go's slice machinery (TinyGo emits a small allocator) and
// return the pointer to the slice's backing array.
//
// Lifetimes: the slice escapes to the heap so the GC keeps it alive
// until dealloc clears it. Using TinyGo's gc=conservative (default for
// wasi) makes this safe — the slice stays reachable through the map
// below.
//
//export alloc
func alloc(size uint32) uintptr {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	live[ptr] = buf
	return ptr
}

//export dealloc
func dealloc(ptr uintptr, _ uint32) {
	delete(live, ptr)
}

// live keeps every host-visible buffer reachable so the GC doesn't
// reclaim them mid-call. The host pairs each alloc with a dealloc so
// the map drains naturally.
var live = map[uintptr][]byte{}

// plugin_manifest returns a JSON-serialized manifest. The host calls
// this immediately after Load to learn the plugin's identity +
// capabilities + tool surface.
//
//export plugin_manifest
func pluginManifest() uint64 {
	body, _ := json.Marshal(map[string]any{
		"id":           "com.example.echo",
		"name":         "Echo Plugin",
		"version":      "0.0.1",
		"author":       "Nomi WASM spike",
		"description":  "Smallest plugin that exercises the Nomi WASM ABI end-to-end.",
		"capabilities": []string{"network.outgoing"},
		"contributes": map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo.echo",
					"capability":  "echo.echo",
					"description": "Returns its input verbatim. Used to validate the WASM round-trip.",
				},
				{
					"name":        "echo.fetch",
					"capability":  "network.outgoing",
					"description": "Performs an HTTP request via host_http_request. Used to validate the gated-import path.",
				},
			},
		},
		"requires": map[string]any{
			"network_allowlist": []string{"127.0.0.1", "*.example.test", "test.invalid"},
		},
	})
	return packResponse(body)
}

// tool_execute dispatches by tool name. echo.echo round-trips the
// input; echo.fetch exercises the gated host_http_request to validate
// the capability gates from the plugin side.
//
//export tool_execute
func toolExecute(ptr uintptr, length uint32) uint64 {
	raw := readBytes(ptr, length)
	var req struct {
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse("invalid input JSON: " + err.Error())
	}
	switch req.Name {
	case "echo.fetch":
		return doFetch(req.Input)
	}
	body, _ := json.Marshal(map[string]any{
		"result": map[string]any{
			"echoed": req.Input,
			"tool":   req.Name,
		},
	})
	return packResponse(body)
}

// doFetch demonstrates the gated host_http_request flow from the
// plugin's perspective: marshal arguments into linear memory, call
// the host import, read back the JSON response. The host returns
// {error: "..."} when the gate denies; {status, body} on success.
func doFetch(input map[string]any) uint64 {
	method, _ := input["method"].(string)
	if method == "" {
		method = "GET"
	}
	urlStr, _ := input["url"].(string)
	if urlStr == "" {
		return errorResponse("echo.fetch: url is required")
	}
	body, _ := input["body"].(string)

	mPtr, mLen := writeStringIntoMemory(method)
	uPtr, uLen := writeStringIntoMemory(urlStr)
	bPtr, bLen := writeStringIntoMemory(body)

	packed := hostHTTPRequest(uint32(mPtr), mLen, uint32(uPtr), uLen, uint32(bPtr), bLen)
	if packed == 0 {
		return errorResponse("echo.fetch: host_http_request returned 0 (likely policy denied)")
	}
	respPtr := uintptr(packed >> 32)
	respLen := uint32(packed & 0xFFFFFFFF)
	respBytes := readBytes(respPtr, respLen)

	// The host writes response bytes via OUR alloc, so the buffer is
	// already in the live map; explicitly free it so the map drains.
	delete(live, respPtr)

	out, _ := json.Marshal(map[string]any{
		"result": map[string]any{
			"raw_response": string(respBytes),
		},
	})
	return packResponse(out)
}

// writeStringIntoMemory allocates a buffer holding s and returns its
// (ptr, len). Mirrors what alloc() does, but reusable inline so
// doFetch reads cleanly.
func writeStringIntoMemory(s string) (uintptr, uint32) {
	if s == "" {
		return 0, 0
	}
	buf := make([]byte, len(s))
	copy(buf, s)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	live[ptr] = buf
	return ptr, uint32(len(s))
}

// readBytes copies the host-supplied buffer into a Go slice we own. We
// can't trust the host pointer to remain valid past this call — the
// host may free it as soon as we return — so a copy is the only safe
// move.
func readBytes(ptr uintptr, length uint32) []byte {
	if length == 0 {
		return nil
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), length)
	out := make([]byte, length)
	copy(out, src)
	return out
}

// packResponse allocates a buffer, copies body in, and returns the
// (ptr<<32 | len) packed value the host's unpackPtrLen expects. The
// host is responsible for calling dealloc once it has read the bytes.
func packResponse(body []byte) uint64 {
	if len(body) == 0 {
		return 0
	}
	buf := make([]byte, len(body))
	copy(buf, body)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	live[ptr] = buf
	return (uint64(ptr) << 32) | uint64(uint32(len(body)))
}

// errorResponse helper for tool_execute's failure paths.
func errorResponse(msg string) uint64 {
	body, _ := json.Marshal(map[string]any{"error": msg})
	return packResponse(body)
}
