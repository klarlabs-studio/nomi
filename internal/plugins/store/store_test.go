package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/plugins/bundle"
)

func freshStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func sampleBundle(id string) *bundle.Bundle {
	return &bundle.Bundle{
		Manifest:    plugins.PluginManifest{ID: id, Name: "Sample", Version: "0.1.0"},
		RawManifest: []byte(`{"id":"` + id + `","name":"Sample","version":"0.1.0"}`),
		WASM:        []byte("\x00asm\x01\x00\x00\x00FAKE"),
		Readme:      []byte("# Sample\n"),
		Signature:   []byte("not-a-real-signature-but-fine-here"),
		Publisher:   bundle.Publisher{Name: "Test", KeyFingerprint: "FP"},
	}
}

func TestInstall_LaysOutFilesAtomically(t *testing.T) {
	s := freshStore(t)
	b := sampleBundle("com.example.x")
	if err := s.Install(b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	dir := filepath.Join(s.Root(), "com.example.x")
	for _, name := range []string{"manifest.json", "plugin.wasm", "publisher.json", "signature.ed25519", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing file %q: %v", name, err)
		}
	}
}

func TestInstall_RefusesDuplicate(t *testing.T) {
	s := freshStore(t)
	b := sampleBundle("com.example.dup")
	if err := s.Install(b); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := s.Install(b); err == nil {
		t.Fatal("expected duplicate install to fail")
	}
}

func TestInstall_RejectsUnsafeID(t *testing.T) {
	s := freshStore(t)
	for _, bad := range []string{"", "../escape", ".hidden", "weird/id", "weird\\id"} {
		b := sampleBundle(bad)
		if err := s.Install(b); err == nil {
			t.Fatalf("expected install to reject id %q", bad)
		}
	}
}

func TestRemove_IsIdempotent(t *testing.T) {
	s := freshStore(t)
	if err := s.Remove("never-installed"); err != nil {
		t.Fatalf("remove non-existent should be nil, got %v", err)
	}
	b := sampleBundle("com.example.r")
	_ = s.Install(b)
	if err := s.Remove("com.example.r"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "com.example.r")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dir still present after Remove: %v", err)
	}
}

func TestWASM_ReturnsInstalledBytes(t *testing.T) {
	s := freshStore(t)
	b := sampleBundle("com.example.w")
	_ = s.Install(b)
	got, err := s.WASM("com.example.w")
	if err != nil {
		t.Fatalf("WASM: %v", err)
	}
	if string(got) != string(b.WASM) {
		t.Fatalf("wasm bytes drifted across install/read")
	}
}

func TestWASM_NotInstalledIsSentinel(t *testing.T) {
	s := freshStore(t)
	_, err := s.WASM("nope")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("expected ErrNotInstalled, got %v", err)
	}
}

func TestList_StableOrderIgnoresStrayDirs(t *testing.T) {
	s := freshStore(t)
	for _, id := range []string{"com.b", "com.a", "com.c"} {
		_ = s.Install(sampleBundle(id))
	}
	// Stray empty + hidden + non-manifest dirs the boot path should ignore.
	_ = os.MkdirAll(filepath.Join(s.Root(), ".tmp-stale"), 0o755)
	_ = os.MkdirAll(filepath.Join(s.Root(), "no-manifest-here"), 0o755)
	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"com.a", "com.b", "com.c"}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List[%d] = %q, want %q (full = %v)", i, got[i], want[i], got)
		}
	}
}

func TestManifest_RoundTrips(t *testing.T) {
	s := freshStore(t)
	b := sampleBundle("com.example.m")
	_ = s.Install(b)
	m, err := s.Manifest("com.example.m")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.ID != "com.example.m" || m.Name != "Sample" {
		t.Fatalf("manifest drifted: %+v", m)
	}
}
