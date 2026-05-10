package main

import (
	"net"
	"testing"
)

func TestIsAddrInUse(t *testing.T) {
	// Hold a real port so the second Listen reproduces a true EADDRINUSE
	// rather than us synthesising a fake error — the helper must work
	// against the actual error chain Go's net stack returns on each OS.
	occupier, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	defer occupier.Close()

	addr := occupier.Addr().String()
	conflict, err := net.Listen("tcp", addr)
	if err == nil {
		conflict.Close()
		t.Fatalf("expected EADDRINUSE binding %s, got nil", addr)
	}
	if !isAddrInUse(err) {
		t.Fatalf("isAddrInUse(%v) = false, want true", err)
	}
	if isAddrInUse(nil) {
		t.Fatal("isAddrInUse(nil) = true, want false")
	}
}
