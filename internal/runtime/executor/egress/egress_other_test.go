//go:build !linux

package egress

import (
	"errors"
	"testing"
)

// On non-Linux platforms New is expected to fail closed with
// ErrUnsupported so the docker backend can detect the unsupported
// case and fall back to the DNS-only allowlist path.
func TestNewReturnsUnsupportedOffLinux(t *testing.T) {
	f, err := New(Config{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if f != nil {
		t.Fatal("expected nil Filter on unsupported platform")
	}
}
