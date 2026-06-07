package bundle

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// validSources returns a fresh, structurally-valid Sources value the
// tests can mutate to break one piece at a time. Centralized so that
// when the bundle format gains required fields, only this helper needs
// to change.
func validSources() Sources {
	return Sources{
		ManifestJSON: []byte(`{
			"id": "com.example.test",
			"name": "Test Plugin",
			"version": "0.1.0",
			"capabilities": ["network.outgoing"],
			"contributes": {},
			"cardinality": "single"
		}`),
		WASM:   []byte("\x00asm\x01\x00\x00\x00"),
		Readme: []byte("# Test plugin\n"),
		PublisherJSON: []byte(`{
			"name": "Nomi Test",
			"key_fingerprint": "AAAA-BBBB-CCCC-DDDD",
			"key_chain": ["root"]
		}`),
		// Signature bytes are opaque to bundle.Open — actual ed25519
		// verification lands in lifecycle-06. For the structural
		// validation tests an arbitrary 64-byte placeholder suffices.
		Signature: bytes.Repeat([]byte{0x42}, 64),
	}
}

// pack invokes Pack on validSources after the caller's mutator runs.
// Returned bytes are a complete .nomi-plugin archive ready to feed
// back into Open.
func pack(t *testing.T, mutate func(*Sources)) []byte {
	t.Helper()
	src := validSources()
	if mutate != nil {
		mutate(&src)
	}
	var buf bytes.Buffer
	if err := Pack(&buf, src); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return buf.Bytes()
}

func TestOpen_ValidBundleRoundTrips(t *testing.T) {
	data := pack(t, nil)
	b, err := Open(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b.Manifest.ID != "com.example.test" {
		t.Fatalf("manifest id = %q, want com.example.test", b.Manifest.ID)
	}
	if !bytes.HasPrefix(b.WASM, []byte("\x00asm")) {
		t.Fatalf("WASM bytes missing magic, got %q", b.WASM[:4])
	}
	if b.Publisher.Name != "Nomi Test" {
		t.Fatalf("publisher name = %q", b.Publisher.Name)
	}
	if len(b.Signature) != 64 {
		t.Fatalf("signature length = %d, want 64", len(b.Signature))
	}
	if b.Hash == "" {
		t.Fatal("Hash empty — content-addressing broken")
	}
}

func TestOpen_HashIsDeterministic(t *testing.T) {
	// Two bundles built from identical sources must produce identical
	// content-address hashes — otherwise the install flow can't dedupe
	// or verify "this is the same artifact NomiHub published."
	a := pack(t, nil)
	b := pack(t, nil)
	ba, err := Open(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	bb, err := Open(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	if ba.Hash != bb.Hash {
		t.Fatalf("hash mismatch on identical sources: %s vs %s", ba.Hash, bb.Hash)
	}
}

func TestOpen_HashChangesWhenWASMChanges(t *testing.T) {
	a := pack(t, nil)
	b := pack(t, func(s *Sources) {
		s.WASM = append(s.WASM, 0xFF) // one byte difference
	})
	ba, _ := Open(bytes.NewReader(a))
	bb, _ := Open(bytes.NewReader(b))
	if ba.Hash == bb.Hash {
		t.Fatal("hash unchanged after WASM payload mutation — content addressing is broken")
	}
}

func TestOpen_RejectsCorruptManifest(t *testing.T) {
	data := pack(t, func(s *Sources) {
		s.ManifestJSON = []byte(`{not valid json`)
	})
	_, err := Open(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected manifest-parse error")
	}
	if !errors.Is(err, ErrCorruptManifest) {
		t.Fatalf("expected ErrCorruptManifest wrap, got %v", err)
	}
}

func TestOpen_RejectsManifestMissingID(t *testing.T) {
	// Structural validity check — having the file present isn't enough
	// if the contents can't act as a plugin manifest.
	data := pack(t, func(s *Sources) {
		s.ManifestJSON = []byte(`{"name": "no id"}`)
	})
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrCorruptManifest) {
		t.Fatalf("expected ErrCorruptManifest for missing id, got %v", err)
	}
}

func TestOpen_RejectsMissingWASM(t *testing.T) {
	data := pack(t, func(s *Sources) { s.WASM = nil })
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrMissingFile) {
		t.Fatalf("expected ErrMissingFile, got %v", err)
	}
	if !strings.Contains(err.Error(), "plugin.wasm") {
		t.Fatalf("error should name plugin.wasm, got %q", err)
	}
}

func TestOpen_RejectsMissingSignature(t *testing.T) {
	data := pack(t, func(s *Sources) { s.Signature = nil })
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrMissingFile) {
		t.Fatalf("expected ErrMissingFile, got %v", err)
	}
	if !strings.Contains(err.Error(), "signature.ed25519") {
		t.Fatalf("error should name signature.ed25519, got %q", err)
	}
}

func TestOpen_RejectsMissingPublisher(t *testing.T) {
	data := pack(t, func(s *Sources) { s.PublisherJSON = nil })
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrMissingFile) {
		t.Fatalf("expected ErrMissingFile, got %v", err)
	}
}

func TestOpen_RejectsCorruptPublisher(t *testing.T) {
	data := pack(t, func(s *Sources) {
		s.PublisherJSON = []byte(`not json`)
	})
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrCorruptPublisher) {
		t.Fatalf("expected ErrCorruptPublisher, got %v", err)
	}
}

func TestOpen_RejectsMissingPublisherFingerprint(t *testing.T) {
	// publisher.json missing key_fingerprint is unusable downstream —
	// signature verification has nothing to look up. Better to refuse
	// at structural validation than fail mysteriously later.
	data := pack(t, func(s *Sources) {
		s.PublisherJSON = []byte(`{"name":"Anon"}`)
	})
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrCorruptPublisher) {
		t.Fatalf("expected ErrCorruptPublisher for missing fingerprint, got %v", err)
	}
}

func TestOpen_RejectsNonGzipInput(t *testing.T) {
	_, err := Open(bytes.NewReader([]byte("not a gzip stream")))
	if err == nil {
		t.Fatal("expected error for non-gzip input")
	}
}

func TestOpen_RejectsTraversalPaths(t *testing.T) {
	// A bundle that tries to smuggle in entries like "../../etc/passwd"
	// or "/abs/path" would be a directory traversal attack on whoever
	// later extracts the bundle. Open must refuse the moment such an
	// entry appears.
	data := pack(t, func(s *Sources) {
		s.ExtraFiles = map[string][]byte{
			"../escape.txt": []byte("nope"),
		}
	})
	_, err := Open(bytes.NewReader(data))
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected ErrUnsafePath, got %v", err)
	}
}

func TestOpen_BoundsBundleSize(t *testing.T) {
	// Bundles larger than maxBundleBytes must be refused without
	// streaming the whole thing into memory — protects the install
	// endpoint from a DoS via a multi-GB upload.
	huge := bytes.Repeat([]byte{0}, maxBundleBytes+1)
	_, err := Open(bytes.NewReader(huge))
	if err == nil {
		t.Fatal("expected size-bound rejection")
	}
}
