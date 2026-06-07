package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
)

// fixedClock returns a now() func that always reports t. Used to make
// expiry tests deterministic without sleeping.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// signedBundleParts is the union of everything needed to construct a
// fully signed test bundle: keys, fingerprint, expiry, payload bytes.
// Centralized so individual tests only override the field they need to
// break.
type signedBundleParts struct {
	rootPub  ed25519.PublicKey
	rootPriv ed25519.PrivateKey
	pubPub   ed25519.PublicKey
	pubPriv  ed25519.PrivateKey

	manifest []byte
	wasm     []byte

	fingerprint string
	expiry      time.Time
}

func defaultParts(t *testing.T) signedBundleParts {
	t.Helper()
	rootPub, rootPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("root keygen: %v", err)
	}
	pubPub, pubPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("publisher keygen: %v", err)
	}
	return signedBundleParts{
		rootPub:     rootPub,
		rootPriv:    rootPriv,
		pubPub:      pubPub,
		pubPriv:     pubPriv,
		manifest:    []byte(`{"id":"com.example.x","name":"X","version":"0.0.1"}`),
		wasm:        []byte("\x00asm\x01\x00\x00\x00FAKE"),
		fingerprint: "AAAA-BBBB-CCCC-DDDD",
		expiry:      time.Now().Add(365 * 24 * time.Hour),
	}
}

// makeBundle assembles a *bundle.Bundle from parts as if it had just
// been parsed by bundle.Open — i.e. with all signatures applied.
// Tests then mutate one field to break exactly one invariant before
// calling Verify.
func makeBundle(p signedBundleParts) *bundle.Bundle {
	return &bundle.Bundle{
		RawManifest: p.manifest,
		WASM:        p.wasm,
		Signature:   Sign(p.pubPriv, p.manifest, p.wasm),
		Publisher: bundle.Publisher{
			Name:           "Test Publisher",
			KeyFingerprint: p.fingerprint,
			PublicKey:      p.pubPub,
			RootSignature:  SignPublisherClaim(p.rootPriv, p.fingerprint, p.pubPub, p.expiry),
			Expiry:         p.expiry,
		},
	}
}

func TestVerify_HappyPath(t *testing.T) {
	p := defaultParts(t)
	v, err := NewVerifier(p.rootPub, fixedClock(time.Now()))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if err := v.Verify(makeBundle(p)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestNewVerifier_RejectsWrongLengthRoot(t *testing.T) {
	_, err := NewVerifier(ed25519.PublicKey{1, 2, 3}, nil)
	if !errors.Is(err, ErrNoRootKey) {
		t.Fatalf("expected ErrNoRootKey, got %v", err)
	}
}

func TestVerify_RejectsTamperedManifest(t *testing.T) {
	// The whole point of signing payload = manifest||wasm: flipping a
	// byte in the manifest must invalidate the bundle signature even
	// though the wasm and the publisher chain are untouched.
	p := defaultParts(t)
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	b := makeBundle(p)
	b.RawManifest = bytes.ReplaceAll(b.RawManifest, []byte("X"), []byte("Y"))
	if err := v.Verify(b); !errors.Is(err, ErrBundleSignatureBad) {
		t.Fatalf("expected ErrBundleSignatureBad after manifest tamper, got %v", err)
	}
}

func TestVerify_RejectsTamperedWASM(t *testing.T) {
	p := defaultParts(t)
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	b := makeBundle(p)
	b.WASM = append(b.WASM, 0xFF)
	if err := v.Verify(b); !errors.Is(err, ErrBundleSignatureBad) {
		t.Fatalf("expected ErrBundleSignatureBad after wasm tamper, got %v", err)
	}
}

func TestVerify_RejectsPublisherSignedByDifferentRoot(t *testing.T) {
	// Hostile actor: built their own root keypair, signed their
	// publisher cert with it, hopes the daemon will accept it. The
	// chain check must fail because our verifier was constructed with
	// a different root.
	p := defaultParts(t)
	otherRoot, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = otherRoot
	p.rootPriv = otherPriv // sign the publisher claim with the wrong key
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	b := makeBundle(p)
	if err := v.Verify(b); !errors.Is(err, ErrRootSignatureInvalid) {
		t.Fatalf("expected ErrRootSignatureInvalid, got %v", err)
	}
}

func TestVerify_RejectsExpiredPublisherKey(t *testing.T) {
	p := defaultParts(t)
	// Issued a year ago, expired six months ago.
	p.expiry = time.Now().Add(-180 * 24 * time.Hour)
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	if err := v.Verify(makeBundle(p)); !errors.Is(err, ErrPublisherKeyExpired) {
		t.Fatalf("expected ErrPublisherKeyExpired, got %v", err)
	}
}

func TestVerify_AllowsZeroExpiry(t *testing.T) {
	// Zero expiry == no expiry. Useful for the embedded NomiHub
	// publisher cert where rotation happens by re-issuing with a new
	// fingerprint, not by setting expirations.
	p := defaultParts(t)
	p.expiry = time.Time{}
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	if err := v.Verify(makeBundle(p)); err != nil {
		t.Fatalf("zero-expiry should be accepted as no-expiry, got %v", err)
	}
}

func TestVerify_RejectsMissingPublisherPubkey(t *testing.T) {
	p := defaultParts(t)
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	b := makeBundle(p)
	b.Publisher.PublicKey = nil
	if err := v.Verify(b); !errors.Is(err, ErrPublisherKeyMissing) {
		t.Fatalf("expected ErrPublisherKeyMissing, got %v", err)
	}
}

func TestVerify_RejectsMissingRootSignature(t *testing.T) {
	p := defaultParts(t)
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	b := makeBundle(p)
	b.Publisher.RootSignature = nil
	if err := v.Verify(b); !errors.Is(err, ErrRootSignatureMissing) {
		t.Fatalf("expected ErrRootSignatureMissing, got %v", err)
	}
}

func TestVerify_RejectsTamperedFingerprint(t *testing.T) {
	// Fingerprint is part of the root-signed claim. If someone tries
	// to swap the fingerprint after the cert was issued (e.g. to
	// impersonate a known-good publisher), the chain check fails.
	p := defaultParts(t)
	v, _ := NewVerifier(p.rootPub, fixedClock(time.Now()))
	b := makeBundle(p)
	b.Publisher.KeyFingerprint = "ZZZZ-ZZZZ-ZZZZ-ZZZZ"
	if err := v.Verify(b); !errors.Is(err, ErrRootSignatureInvalid) {
		t.Fatalf("expected ErrRootSignatureInvalid after fingerprint swap, got %v", err)
	}
}

func TestPayloadHash_StableAcrossRuns(t *testing.T) {
	a := PayloadHash([]byte("manifest"), []byte("wasm"))
	b := PayloadHash([]byte("manifest"), []byte("wasm"))
	if a != b {
		t.Fatal("PayloadHash not deterministic")
	}
	c := PayloadHash([]byte("manifest"), []byte("wasm2"))
	if a == c {
		t.Fatal("PayloadHash collision on different wasm")
	}
}
