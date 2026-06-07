// Package tunnel provides pluggable tunnel adapters that expose the local
// Nomi API to the internet via a stable HTTPS URL. The default adapter
// uses ngrok-go; a no-op fallback is available when tunneling is disabled.
package tunnel

import (
	"context"
	"fmt"
)

// Adapter is the interface for tunnel implementations.
type Adapter interface {
	// Start opens the tunnel and returns the public HTTPS URL.
	// The tunnel forwards all traffic to localAddr (e.g. "127.0.0.1:8080").
	Start(ctx context.Context, localAddr string) (string, error)

	// Stop closes the tunnel.
	Stop() error

	// URL returns the current public URL, or "" if not started.
	URL() string
}

// Kind enumerates supported tunnel adapters.
type Kind string

const (
	KindNgrok Kind = "ngrok"
	KindNoop  Kind = "noop"
)

// Config holds user-facing tunnel settings persisted in app_settings.
type Config struct {
	Kind      Kind   `json:"kind"`
	AuthToken string `json:"auth_token,omitempty"` // ngrok authtoken (secret:// ref)
	Domain    string `json:"domain,omitempty"`     // optional fixed ngrok domain
}

// New creates a tunnel adapter from config.
func New(cfg Config) (Adapter, error) {
	switch cfg.Kind {
	case KindNgrok:
		return newNgrokAdapter(cfg), nil
	case KindNoop:
		return &noopAdapter{}, nil
	default:
		return nil, fmt.Errorf("unknown tunnel kind %q", cfg.Kind)
	}
}
