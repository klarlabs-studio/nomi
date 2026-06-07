package tunnel

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"
)

// ngrokAdapter implements Adapter using the official ngrok-go SDK.
type ngrokAdapter struct {
	cfg       Config
	url       string
	forwarder ngrok.Forwarder
	mu        sync.RWMutex
}

func newNgrokAdapter(cfg Config) *ngrokAdapter {
	return &ngrokAdapter{cfg: cfg}
}

// Start opens an ngrok tunnel forwarding to localAddr.
// If cfg.AuthToken is empty, ngrok runs in unauthenticated mode
// (ephemeral URLs, session lifetime ~2h).
func (a *ngrokAdapter) Start(ctx context.Context, localAddr string) (string, error) {
	opts := []ngrok.ConnectOption{}
	if a.cfg.AuthToken != "" {
		opts = append(opts, ngrok.WithAuthtoken(a.cfg.AuthToken))
	}

	fwdOpts := []config.HTTPEndpointOption{}
	if a.cfg.Domain != "" {
		fwdOpts = append(fwdOpts, config.WithDomain(a.cfg.Domain))
	}

	u, err := url.Parse("http://" + localAddr)
	if err != nil {
		return "", fmt.Errorf("parse local addr: %w", err)
	}

	log.Printf("[tunnel] Starting ngrok tunnel to %s...", localAddr)

	fwd, err := ngrok.ListenAndForward(ctx,
		u,
		config.HTTPEndpoint(fwdOpts...),
		opts...,
	)
	if err != nil {
		return "", fmt.Errorf("ngrok tunnel: %w", err)
	}

	a.mu.Lock()
	a.forwarder = fwd
	a.url = fwd.URL()
	a.mu.Unlock()

	log.Printf("[tunnel] ngrok tunnel active: %s", a.url)
	return a.url, nil
}

// Stop closes the ngrok tunnel.
func (a *ngrokAdapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.forwarder != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := a.forwarder.CloseWithContext(ctx)
		a.forwarder = nil
		a.url = ""
		return err
	}
	return nil
}

// URL returns the public HTTPS URL.
func (a *ngrokAdapter) URL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.url
}
