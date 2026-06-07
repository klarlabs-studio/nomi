// Package signing implements the bundle signature chain described in
// ADR 0002 §2 (marketplace tier) and lifecycle-06.
//
// The trust model is a fixed two-level chain:
//
//	root pubkey (embedded in daemon)
//	   │
//	   │ signs publisher pubkey + fingerprint + expiry  →  RootSignature
//	   ▼
//	publisher pubkey
//	   │
//	   │ signs SHA-256(manifest.json || plugin.wasm)    →  bundle Signature
//	   ▼
//	bundle (manifest + wasm + signature.ed25519)
//
// Verify walks the chain bottom-up: hashes the payload, checks the
// publisher signature, then checks the root signature over the
// publisher's claim. Anything missing or malformed → reject.
//
// Why DI rather than embedded constants for the root key: the daemon
// embeds the production NomiHub root via go:embed at the call site;
// tests construct verifiers with generated keys; dev/CI never need to
// rotate a build-time constant. Same package, different keys, no build
// tags.
package signing

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"go.klarlabs.de/nomi/internal/plugins/bundle"
)

// Sentinel errors for branchable failure cases. The install dialog and
// dev loader display different messages per cause, so all callers need
// to be able to distinguish them via errors.Is.
var (
	ErrNoRootKey            = errors.New("signing: verifier built without a root pubkey")
	ErrPublisherKeyMissing  = errors.New("signing: publisher.json missing public_key")
	ErrPublisherKeyExpired  = errors.New("signing: publisher key has expired")
	ErrRootSignatureMissing = errors.New("signing: publisher.json missing root_signature")
	ErrRootSignatureInvalid = errors.New("signing: publisher key is not signed by the root key")
	ErrBundleSignatureBad   = errors.New("signing: bundle signature does not match payload")
)

// Verifier holds the trust anchor (root pubkey) and a clock function
// the chain checks consult. Production builds inject time.Now; tests
// inject a frozen clock so expiry tests are deterministic.
type Verifier struct {
	root ed25519.PublicKey
	now  func() time.Time
}

// NewVerifier returns a Verifier with the given root pubkey. now may
// be nil — defaults to time.Now.
func NewVerifier(root ed25519.PublicKey, now func() time.Time) (*Verifier, error) {
	if len(root) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: got %d bytes, want %d", ErrNoRootKey, len(root), ed25519.PublicKeySize)
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{root: root, now: now}, nil
}

// PayloadHash is the canonical input the publisher signs: SHA-256 over
// manifest.json bytes concatenated with plugin.wasm bytes. Exposed so
// publisher tooling (out-of-tree) can compute the same hash without
// importing the verify path.
func PayloadHash(manifestJSON, wasm []byte) [32]byte {
	h := sha256.New()
	h.Write(manifestJSON)
	h.Write(wasm)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// rootSignedClaim is the byte sequence the root key signs to attest a
// publisher's identity. Includes the fingerprint, the public key, and
// the expiry — so swapping any of those invalidates the signature.
//
// Format (length-prefixed for unambiguous parsing):
//
//	uint32(len(fingerprint)) || fingerprint
//	uint32(len(pubkey))      || pubkey
//	int64(expiry unix sec)
//
// A custom format keeps us off ASN.1 / X.509 — overkill for two
// levels — while remaining unambiguous and deterministic.
func rootSignedClaim(fingerprint string, pubkey ed25519.PublicKey, expiry time.Time) []byte {
	fpBytes := []byte(fingerprint)
	out := make([]byte, 0, 4+len(fpBytes)+4+len(pubkey)+8)

	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(len(fpBytes)))
	out = append(out, u32[:]...)
	out = append(out, fpBytes...)

	binary.BigEndian.PutUint32(u32[:], uint32(len(pubkey)))
	out = append(out, u32[:]...)
	out = append(out, pubkey...)

	var i64 [8]byte
	binary.BigEndian.PutUint64(i64[:], uint64(expiry.Unix()))
	out = append(out, i64[:]...)

	return out
}

// Verify walks the chain on a parsed bundle. Returns nil only when
// every check passes. Caller (install endpoint, dev loader) should
// surface the wrapped sentinel via errors.Is for UI dialog selection.
//
// The bundle parameter accepts a *bundle.Bundle so callers don't need
// to redundantly pass manifest+wasm separately — they're already in
// the parsed Bundle, and using the typed value forces structural
// validation to have already happened.
func (v *Verifier) Verify(b *bundle.Bundle) error {
	if v == nil || len(v.root) == 0 {
		return ErrNoRootKey
	}
	if len(b.Publisher.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: got %d bytes", ErrPublisherKeyMissing, len(b.Publisher.PublicKey))
	}
	if len(b.Publisher.RootSignature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: got %d bytes", ErrRootSignatureMissing, len(b.Publisher.RootSignature))
	}
	expiry := b.Publisher.Expiry
	if !expiry.IsZero() && v.now().After(expiry) {
		return fmt.Errorf("%w: expired at %s", ErrPublisherKeyExpired, expiry.Format(time.RFC3339))
	}
	claim := rootSignedClaim(b.Publisher.KeyFingerprint, b.Publisher.PublicKey, expiry)
	if !ed25519.Verify(v.root, claim, b.Publisher.RootSignature) {
		return ErrRootSignatureInvalid
	}
	manifestBytes, err := bundle.CanonicalManifestBytes(b)
	if err != nil {
		return fmt.Errorf("signing: re-canonicalize manifest: %w", err)
	}
	hash := PayloadHash(manifestBytes, b.WASM)
	if !ed25519.Verify(b.Publisher.PublicKey, hash[:], b.Signature) {
		return ErrBundleSignatureBad
	}
	return nil
}

// Sign is the publisher-side helper: given the publisher's private key
// and the raw payload bytes, produces a signature suitable for
// signature.ed25519 inside a bundle. Lives in this package so the
// publisher tooling and the verifier can never drift apart on the hash
// definition.
func Sign(publisherKey ed25519.PrivateKey, manifestJSON, wasm []byte) []byte {
	hash := PayloadHash(manifestJSON, wasm)
	return ed25519.Sign(publisherKey, hash[:])
}

// SignPublisherClaim is the root-key holder's helper: produces the
// root_signature value to embed in publisher.json. Lives here for the
// same drift-prevention reason as Sign.
func SignPublisherClaim(rootKey ed25519.PrivateKey, fingerprint string, publisherPubkey ed25519.PublicKey, expiry time.Time) []byte {
	claim := rootSignedClaim(fingerprint, publisherPubkey, expiry)
	return ed25519.Sign(rootKey, claim)
}
