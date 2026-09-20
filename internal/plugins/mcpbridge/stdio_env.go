package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"

	mcpclient "go.klarlabs.de/mcp/client"
	"go.klarlabs.de/mcp/protocol"
	"go.klarlabs.de/mcp/transport"
)

// stdioEnvTransport is a local stdio MCP transport that injects a custom
// environment before spawning. Upstream go.klarlabs.de/mcp@v1.22.0
// NewStdioTransport has no WithEnv option yet; this keeps secrets out of
// SQLite while still speaking the same newline-JSON wire format.
type stdioEnvTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	framer *transport.NewlineFramer

	mu       sync.Mutex
	respChan map[int64]chan *protocol.Response
	closed   bool
	readWG   sync.WaitGroup
}

// Compile-time check.
var _ mcpclient.Transport = (*stdioEnvTransport)(nil)

func newStdioTransportWithEnv(command string, args []string, env []string) (*stdioEnvTransport, error) {
	resolved, err := mcpclient.ValidateCommand(command)
	if err != nil {
		return nil, fmt.Errorf("validate command: %w", err)
	}
	if err := mcpclient.ValidateArgs(args); err != nil {
		return nil, fmt.Errorf("validate args: %w", err)
	}

	cmd := exec.Command(resolved, args...) //nolint:gosec // validated above
	if len(env) > 0 {
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	t := &stdioEnvTransport{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		respChan: map[int64]chan *protocol.Response{},
		framer:   transport.NewNewlineFramer(stdout, stdin),
	}
	t.readWG.Add(1)
	go t.readResponses()
	return t, nil
}

func (t *stdioEnvTransport) Send(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("transport closed")
	}
	var id int64
	if err := json.Unmarshal(req.ID, &id); err != nil {
		t.mu.Unlock()
		return nil, fmt.Errorf("invalid request ID: %w", err)
	}
	respCh := make(chan *protocol.Response, 1)
	t.respChan[id] = respCh
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.respChan, id)
		t.mu.Unlock()
	}()

	if err := t.framer.WriteMessage(req); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		return resp, nil
	}
}

func (t *stdioEnvTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	_ = t.stdin.Close()
	t.readWG.Wait()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	return t.cmd.Wait()
}

func (t *stdioEnvTransport) readResponses() {
	defer t.readWG.Done()
	for {
		line, err := t.framer.ReadMessage()
		if err != nil {
			if errors.Is(err, transport.ErrFrameTooLarge) {
				continue
			}
			return
		}
		var resp protocol.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		var id int64
		if err := json.Unmarshal(resp.ID, &id); err != nil {
			continue
		}
		t.mu.Lock()
		if ch, ok := t.respChan[id]; ok {
			ch <- &resp
		}
		t.mu.Unlock()
	}
}
