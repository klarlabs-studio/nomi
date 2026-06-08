package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Client is the thin HTTP wrapper every subcommand uses. URL + token
// resolution honours the same precedence the desktop shell uses, so
// the CLI "just works" on the same machine as the daemon.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// NewClient resolves the URL and token, falling back to api.endpoint /
// auth.token written by the daemon at startup. Returns an actionable
// error if neither flag nor file resolves to a usable value — better
// than a confused HTTP 401 on the first call.
func NewClient(c *commonFlags) (*Client, error) {
	url := resolveURL(c.URL)
	tok, err := resolveToken(c.Token)
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}
	return &Client{
		URL:   strings.TrimRight(url, "/"),
		Token: tok,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func resolveURL(flagURL string) string {
	if flagURL != "" {
		return flagURL
	}
	// Read api.endpoint written by the daemon. JSON of the form
	// {"url":"http://127.0.0.1:8080","port":"8080"}.
	if path := dataDirPath("api.endpoint"); path != "" {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: app-internal data-dir endpoint file
			var ep struct{ URL string }
			if json.Unmarshal(data, &ep) == nil && ep.URL != "" {
				return ep.URL
			}
		}
	}
	return "http://127.0.0.1:8080"
}

func resolveToken(flagTok string) (string, error) {
	if flagTok != "" {
		return flagTok, nil
	}
	if v := os.Getenv("NOMI_TOKEN"); v != "" {
		return strings.TrimSpace(v), nil
	}
	if path := dataDirPath("auth.token"); path != "" {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: app-internal data-dir endpoint file
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", fmt.Errorf("no token: pass --token, set $NOMI_TOKEN, or run on the same host as nomid")
}

// dataDirPath mirrors nomid's appDataDir() resolution so the CLI finds
// auth.token / api.endpoint without the user having to specify paths.
func dataDirPath(name string) string {
	if dir := os.Getenv("NOMI_DATA_DIR"); dir != "" {
		return filepath.Join(dir, name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Nomi", name)
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "Nomi", name)
		}
	}
	// Linux + fallback: XDG_CONFIG_HOME or ~/.config
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "Nomi", name)
	}
	return filepath.Join(home, ".config", "Nomi", name)
}

// do sends a request with the bearer token attached. Non-2xx responses
// surface as ApiError so subcommand code can pattern-match on the
// specific error shape (e.g. "no provider configured" vs "policy deny").
func (c *Client) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.URL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, string(raw))
	}
	return raw, nil
}

func (c *Client) Get(path string, out any) error {
	raw, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) Post(path string, body, out any) error {
	raw, err := c.do(http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Client) Put(path string, body, out any) error {
	raw, err := c.do(http.MethodPut, path, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// newBufferReader wraps a byte slice in an io.Reader. Used by raw-YAML
// requests where bytes.NewReader would force the caller to import
// `bytes`; keeping it here gives subcommand files a smaller import set.
func newBufferReader(b []byte) io.Reader {
	return &byteReader{data: b}
}

type byteReader struct {
	data []byte
	off  int
}

func (b *byteReader) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

// printJSON formats v as indented JSON to stdout. Used when --json is
// passed; subcommand code falls back to its own table renderer
// otherwise.
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
