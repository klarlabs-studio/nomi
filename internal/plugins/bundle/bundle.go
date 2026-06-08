// Package bundle implements the .nomi-plugin distribution format
// defined in ADR 0002 §2 (marketplace tier). A bundle is a gzipped tar
// archive containing five fixed entries:
//
//	manifest.json       — the PluginManifest (id, version, capabilities, …)
//	plugin.wasm         — the WebAssembly binary the wasmhost loader runs
//	README.md           — surfaced verbatim in the install dialog
//	signature.ed25519   — 64-byte detached signature over the canonical archive
//	publisher.json      — publisher identity + fingerprint chained to NomiHub root
//
// Open() validates structural integrity (all five entries present,
// manifest parseable, publisher has a key fingerprint, no path
// traversal) and returns a Bundle ready for downstream signature
// verification (lifecycle-06) and install (lifecycle-07).
//
// Pack() is the symmetric writer used by tests and by the future
// publisher tooling. It writes a deterministic archive (sorted entries,
// zero mtimes, fixed mode) so the content-addressed hash is stable
// across rebuilds — the install flow uses this hash to dedupe and
// verify "this is the bytes NomiHub published."
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/nomi/internal/plugins"
)

// zeroTime returns the canonical mtime baked into every tar header so
// two packs from the same Sources produce byte-identical archives.
// time.Time{} would also work but the Unix epoch is what the
// reproducible-builds community has standardized on.
func zeroTime() time.Time { return time.Unix(0, 0).UTC() }

const (
	// maxBundleBytes caps the size of a bundle the install path will
	// consider. 32 MiB comfortably fits any reasonable plugin (the
	// std-Go echo bench above is 3 MiB; TinyGo plugins are sub-MiB)
	// while keeping a single malicious upload from exhausting memory.
	maxBundleBytes = 32 * 1024 * 1024

	manifestFile  = "manifest.json"
	wasmFile      = "plugin.wasm"
	readmeFile    = "README.md"
	signatureFile = "signature.ed25519"
	publisherFile = "publisher.json"
)

// Sentinel errors so callers (install endpoint, dev loader, dialog UI)
// can branch on cause without string matching. Wrap with %w when
// returning so errors.Is reports the right reason.
var (
	ErrCorruptManifest  = errors.New("bundle: manifest.json corrupt or missing required fields")
	ErrCorruptPublisher = errors.New("bundle: publisher.json corrupt or missing required fields")
	ErrMissingFile      = errors.New("bundle: required entry missing")
	ErrUnsafePath       = errors.New("bundle: archive contains unsafe path")
)

// Sources is the input to Pack — one in-memory copy of every required
// entry plus a slot for any extra files (e.g. icon assets, fixture
// inputs the test pack uses to inject path-traversal entries).
type Sources struct {
	ManifestJSON  []byte
	WASM          []byte
	Readme        []byte
	Signature     []byte
	PublisherJSON []byte
	// ExtraFiles is keyed by tar path; values are the file bytes.
	// Used by tests to construct adversarial bundles. Production
	// publishers add icon assets etc. via this slot.
	ExtraFiles map[string][]byte
}

// Publisher mirrors the on-disk publisher.json. The chain anchored at
// the embedded NomiHub root key is:
//
//   - PublicKey       — Ed25519 pubkey the bundle Signature was made with
//   - RootSignature   — root key's Ed25519 signature over (fingerprint,
//     PublicKey, Expiry); proves the publisher was
//     attested by NomiHub
//   - KeyFingerprint  — short human-readable id surfaced in the install
//     dialog (e.g. "AAAA-BBBB-CCCC-DDDD")
//   - Expiry          — optional; rejected by the verifier once past
//   - KeyChain        — informational, recorded for audit (typically
//     just ["root"] for v1)
//
// PublicKey + RootSignature are base64-encoded on disk so publisher.json
// stays human-readable; the json:"" tags decode to []byte via
// encoding/json's standard base64 handling for byte slices.
type Publisher struct {
	Name           string    `json:"name"`
	KeyFingerprint string    `json:"key_fingerprint"`
	PublicKey      []byte    `json:"public_key,omitempty"`
	RootSignature  []byte    `json:"root_signature,omitempty"`
	Expiry         time.Time `json:"expiry,omitempty"`
	KeyChain       []string  `json:"key_chain,omitempty"`
}

// Bundle is the parsed result of Open. WASM and Signature are raw
// bytes; signature verification happens in a separate package so this
// one stays free of crypto dependencies (easier to test, smaller
// blast radius if the bundle parser turns out to have a bug).
type Bundle struct {
	Manifest plugins.PluginManifest
	// RawManifest is the manifest.json bytes exactly as they appeared
	// in the bundle. Re-serializing the parsed Manifest would lose
	// whitespace and key order, so signature verification — which
	// hashes manifest.json || plugin.wasm — must use these raw bytes,
	// not the marshaled Manifest. Accessed via CanonicalManifestBytes.
	RawManifest []byte
	WASM        []byte
	Readme      []byte
	Signature   []byte
	Publisher   Publisher
	// Hash is the hex-encoded SHA-256 of the canonical archive bytes
	// (the input gzipped tar, before extraction). Stable across
	// rebuilds because Pack writes deterministically. Use to dedupe
	// downloads and to confirm bytes-equal-to-published.
	Hash string
}

// CanonicalManifestBytes returns the manifest.json bytes that were in
// the bundle when it was signed. Used by the signing package's Verify
// to reproduce the publisher's payload hash without re-serializing
// (which would change the bytes and break the signature).
func CanonicalManifestBytes(b *Bundle) ([]byte, error) {
	if b == nil || len(b.RawManifest) == 0 {
		return nil, fmt.Errorf("bundle: no raw manifest available")
	}
	return b.RawManifest, nil
}

// Open reads, validates, and parses a .nomi-plugin archive.
// Bound at maxBundleBytes: input is wrapped in an io.LimitReader so a
// pathological gzip bomb can't blow the heap.
func Open(r io.Reader) (*Bundle, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxBundleBytes+1))
	if err != nil {
		return nil, fmt.Errorf("bundle: read: %w", err)
	}
	if len(raw) > maxBundleBytes {
		return nil, fmt.Errorf("bundle: exceeds %d byte limit", maxBundleBytes)
	}
	hash := sha256.Sum256(raw)
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("bundle: gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	files := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: tar: %w", err)
		}
		if !safePath(hdr.Name) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, hdr.Name)
		}
		// Skip directories — we don't model them and they shouldn't
		// appear in a deterministic Pack output anyway.
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxBundleBytes))
		if err != nil {
			return nil, fmt.Errorf("bundle: read entry %q: %w", hdr.Name, err)
		}
		files[hdr.Name] = body
	}

	for _, required := range []string{manifestFile, wasmFile, signatureFile, publisherFile} {
		if _, ok := files[required]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrMissingFile, required)
		}
	}

	manifest, err := parseManifest(files[manifestFile])
	if err != nil {
		return nil, err
	}
	publisher, err := parsePublisher(files[publisherFile])
	if err != nil {
		return nil, err
	}

	return &Bundle{
		Manifest:    manifest,
		RawManifest: files[manifestFile],
		WASM:        files[wasmFile],
		Readme:      files[readmeFile], // optional in spirit, encouraged in practice
		Signature:   files[signatureFile],
		Publisher:   publisher,
		Hash:        hex.EncodeToString(hash[:]),
	}, nil
}

func parseManifest(body []byte) (plugins.PluginManifest, error) {
	var m plugins.PluginManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("%w: %v", ErrCorruptManifest, err)
	}
	if m.ID == "" {
		return m, fmt.Errorf("%w: id is required", ErrCorruptManifest)
	}
	return m, nil
}

func parsePublisher(body []byte) (Publisher, error) {
	var p Publisher
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("%w: %v", ErrCorruptPublisher, err)
	}
	if p.KeyFingerprint == "" {
		return p, fmt.Errorf("%w: key_fingerprint is required", ErrCorruptPublisher)
	}
	return p, nil
}

// safePath rejects entries that would escape the bundle root once
// extracted. Strict allow-list: relative path, no leading slash, no
// `..` component, no NUL byte. Backslashes are a Windows-only path
// separator we don't handle, so reject those too.
func safePath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\x00\\") {
		return false
	}
	if strings.HasPrefix(p, "/") {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// Pack writes a deterministic gzipped tar containing all required
// entries plus any ExtraFiles. Determinism rules (so the hash is
// stable): entries sorted by path, mtime zeroed, fixed file mode,
// gzip header without filename/timestamp.
func Pack(w io.Writer, src Sources) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	// Strip the gzip header's name + timestamp so two packs from the
	// same Sources produce identical bytes.
	gz.ModTime = zeroTime()
	gz.Name = ""
	gz.OS = 0
	defer func() { _ = gz.Close() }()

	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	entries := map[string][]byte{}
	if src.ManifestJSON != nil {
		entries[manifestFile] = src.ManifestJSON
	}
	if src.WASM != nil {
		entries[wasmFile] = src.WASM
	}
	if src.Readme != nil {
		entries[readmeFile] = src.Readme
	}
	if src.Signature != nil {
		entries[signatureFile] = src.Signature
	}
	if src.PublisherJSON != nil {
		entries[publisherFile] = src.PublisherJSON
	}
	for name, body := range src.ExtraFiles {
		entries[name] = body
	}

	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		body := entries[name]
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: zeroTime(),
			Format:  tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	return nil
}
