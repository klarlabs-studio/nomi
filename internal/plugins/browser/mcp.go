// Package browser is the system-tier plugin that gives Nomi
// assistants browser-automation tools by delegating to Scout
// (https://github.com/klarlabs-studio/scout) over stdio MCP. Scout
// owns the browser lifecycle, semantic observation, and Playwright
// integration; the Nomi plugin is a thin contract translator that
// proxies tool calls into Scout's MCP surface.
//
// The choice of Scout (vs embedding chromedp/playwright-go directly)
// was settled in browser-01 — see ADR notes in roady evidence — and
// boils down to: Scout already solves the hard parts (process
// management, isolated profiles, semantic observers), so duplicating
// it inside nomid would be ceremony with no benefit. The trade-off
// is an external binary dependency the user must install once
// (brew install scout / npm install -g scout).
package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// mcpClient is a minimal JSON-RPC 2.0 over stdio MCP client. Only
// implements what the browser plugin needs: initialize handshake,
// tools/list, tools/call. Notifications + sampling + the rest of
// MCP are out of scope — Scout doesn't push anything we care about.
//
// Concurrency: Send is goroutine-safe; the reader runs in a
// dedicated goroutine and routes responses back via per-id channels.
type mcpClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	encoder *json.Encoder

	// nextID generates monotonically increasing JSON-RPC ids. atomic
	// because Send may be called from multiple goroutines concurrently.
	nextID atomic.Int64

	// pending holds the response channel for each in-flight request.
	// The reader goroutine drops responses into the matching channel;
	// Send blocks until the channel receives or the context cancels.
	pendingMu sync.Mutex
	pending   map[int64]chan *jsonRPCResponse

	// closed indicates the client has been Closed and Send should fail
	// fast rather than wait for a response that will never arrive.
	closedMu sync.Mutex
	closed   bool
	closeErr error
}

// jsonRPCRequest / jsonRPCResponse / jsonRPCError model the wire
// shape. ID is int64-only (string ids are valid in spec but Scout
// emits ints and supporting both forces interface{} typing that
// complicates the routing map).
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// startMCP spawns the Scout binary in MCP-stdio mode, performs the
// initialize handshake, and returns a client ready for tools/call.
// The caller is responsible for Close() at shutdown.
//
// binPath is the scout executable to invoke; args are passed
// straight through (typically `["mcp", "serve"]`). Both come from
// the connection config so users with non-standard installs can
// override.
func startMCP(ctx context.Context, binPath string, args []string) (*mcpClient, error) {
	if binPath == "" {
		return nil, errors.New("browser: scout binary path required")
	}

	cmd := exec.CommandContext(ctx, binPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("browser: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("browser: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("browser: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("browser: start scout: %w", err)
	}

	c := &mcpClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		encoder: json.NewEncoder(stdin),
		pending: map[int64]chan *jsonRPCResponse{},
	}

	go c.readLoop()
	// Drain stderr to a no-op sink so a chatty Scout doesn't block on
	// a full pipe buffer. Real log routing comes later via host_log.
	go drainStderr(stderr)

	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("browser: initialize: %w", err)
	}
	return c, nil
}

// initialize is the MCP handshake — sends initialize then the
// initialized notification. Without this handshake Scout refuses
// tool calls.
func (c *mcpClient) initialize(ctx context.Context) error {
	type initParams struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		ClientInfo      map[string]string      `json:"clientInfo"`
		Capabilities    map[string]interface{} `json:"capabilities"`
	}
	_, err := c.call(ctx, "initialize", initParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      map[string]string{"name": "nomid-browser-plugin", "version": "0.1.0"},
		Capabilities:    map[string]interface{}{},
	})
	if err != nil {
		return err
	}
	// initialized is a notification (no id, no response).
	if err := c.encoder.Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}); err != nil {
		return fmt.Errorf("send initialized: %w", err)
	}
	return nil
}

// CallTool invokes a Scout MCP tool by name with the given JSON
// arguments and returns the raw result payload.
//
// MCP tools/call result shape is `{content: [{type: "text", text:
// "..."}]}`. The plugin layer above unmarshals each tool's response
// into the typed shape it expects.
func (c *mcpClient) CallTool(ctx context.Context, name string, args map[string]interface{}) (json.RawMessage, error) {
	type toolCallParams struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments,omitempty"`
	}
	return c.call(ctx, "tools/call", toolCallParams{Name: name, Arguments: args})
}

// call sends a request and blocks until the response arrives or the
// context cancels.
func (c *mcpClient) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.closedMu.Lock()
	if c.closed {
		err := c.closeErr
		c.closedMu.Unlock()
		if err == nil {
			err = errors.New("browser: mcp client closed")
		}
		return nil, err
	}
	c.closedMu.Unlock()

	id := c.nextID.Add(1)
	ch := make(chan *jsonRPCResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	// Encode + write must be serialized — the json encoder writes to
	// a single stdin pipe and concurrent writes interleave bytes.
	// Holding pendingMu across the write would block the read loop,
	// so use a dedicated send mutex.
	if err := c.send(jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

var sendMu sync.Mutex

func (c *mcpClient) send(req jsonRPCRequest) error {
	sendMu.Lock()
	defer sendMu.Unlock()
	if err := c.encoder.Encode(req); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return nil
}

// readLoop is the long-running stdout consumer. Each line is a
// JSON-RPC message; we route responses to their matching pending
// channel and ignore notifications (Scout doesn't send any we need).
func (c *mcpClient) readLoop() {
	scanner := bufio.NewScanner(c.stdout)
	// Default buffer is 64 KiB — Scout's annotated_screenshot
	// response can be much bigger (image bytes inline). Bump to 16
	// MiB which comfortably fits a full-page screenshot.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // malformed line; nothing useful to do
		}
		if resp.ID == 0 {
			continue // notification — Scout doesn't send any we care about
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		c.pendingMu.Unlock()
		if !ok {
			continue
		}
		ch <- &resp
	}
	// Reader exited: stdout closed (process died) or read error.
	c.closeWithErr(fmt.Errorf("scout subprocess exited: %w", scanner.Err()))
}

// closeWithErr marks the client failed and unblocks every pending
// caller with the error. Idempotent.
func (c *mcpClient) closeWithErr(err error) {
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	c.closedMu.Unlock()

	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- &jsonRPCResponse{ID: id, Error: &jsonRPCError{Code: -32603, Message: err.Error()}}
		close(ch)
	}
	c.pending = nil
	c.pendingMu.Unlock()
}

// Close terminates the Scout subprocess and frees resources. Safe
// to call multiple times.
func (c *mcpClient) Close() error {
	c.closeWithErr(errors.New("client closed"))
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// Give Scout a moment to clean up gracefully (close browser,
		// flush logs); kill if it hangs.
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
			<-done
		case <-done:
		}
	}
	return nil
}

func drainStderr(r io.ReadCloser) {
	buf := make([]byte, 4096)
	for {
		_, err := r.Read(buf)
		if err != nil {
			return
		}
	}
}
