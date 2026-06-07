package obsidian

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/tools"
)

func TestDiscoverVault_RejectsMissingPath(t *testing.T) {
	_, err := DiscoverVault(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestDiscoverVault_RejectsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.md")
	mustWrite(t, file, "hello")

	_, err := DiscoverVault(file)
	if !errors.Is(err, ErrVaultNotADirectory) {
		t.Fatalf("got %v, want ErrVaultNotADirectory", err)
	}
}

func TestDiscoverVault_EmptyVault(t *testing.T) {
	dir := t.TempDir()

	meta, err := DiscoverVault(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if meta.HasObsidianConfig {
		t.Fatal("empty dir should not be flagged as configured")
	}
	if meta.NoteCount != 0 {
		t.Fatalf("note count: got %d, want 0", meta.NoteCount)
	}
	if len(meta.Tags) != 0 {
		t.Fatalf("tags: got %v, want []", meta.Tags)
	}
}

func TestDiscoverVault_DetectsObsidianConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".obsidian"), 0o755); err != nil {
		t.Fatalf("mkdir .obsidian: %v", err)
	}

	meta, err := DiscoverVault(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !meta.HasObsidianConfig {
		t.Fatal("expected HasObsidianConfig=true when .obsidian exists")
	}
}

func TestDiscoverVault_CountsMarkdownAndSkipsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "note1.md"), "")
	mustWrite(t, filepath.Join(dir, "note2.MD"), "") // case-insensitive
	mustWrite(t, filepath.Join(dir, "image.png"), "")
	mustWrite(t, filepath.Join(dir, "readme.txt"), "")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "sub", "nested.md"), "")

	meta, err := DiscoverVault(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if meta.NoteCount != 3 {
		t.Fatalf("note count: got %d, want 3", meta.NoteCount)
	}
}

func TestDiscoverVault_SkipsHiddenDirectories(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "visible.md"), "")
	if err := os.MkdirAll(filepath.Join(dir, ".obsidian"), 0o755); err != nil {
		t.Fatalf("mkdir .obsidian: %v", err)
	}
	mustWrite(t, filepath.Join(dir, ".obsidian", "workspace.md"), "")
	if err := os.MkdirAll(filepath.Join(dir, ".trash"), 0o755); err != nil {
		t.Fatalf("mkdir .trash: %v", err)
	}
	mustWrite(t, filepath.Join(dir, ".trash", "deleted.md"), "")

	meta, err := DiscoverVault(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if meta.NoteCount != 1 {
		t.Fatalf("hidden directories must be skipped: got %d, want 1", meta.NoteCount)
	}
}

func TestDiscoverVault_ExtractsFrontmatterTags(t *testing.T) {
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "inline.md"), `---
title: Inline list
tags: [research, ai, "machine learning"]
---

body
`)
	mustWrite(t, filepath.Join(dir, "block.md"), `---
title: Block list
tags:
  - research
  - personal
  - "writing"
---

body
`)
	mustWrite(t, filepath.Join(dir, "singular.md"), `---
tag: solo
---

body
`)
	mustWrite(t, filepath.Join(dir, "no-frontmatter.md"), `# Just content

no frontmatter here
`)
	mustWrite(t, filepath.Join(dir, "leading-hash.md"), `---
tags: [#archived]
---
`)

	meta, err := DiscoverVault(dir)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	want := []string{
		"ai",
		"archived",
		"machine learning",
		"personal",
		"research",
		"solo",
		"writing",
	}
	if !reflect.DeepEqual(meta.Tags, want) {
		t.Fatalf("tags:\n got  %v\n want %v", meta.Tags, want)
	}
}

func TestResolveInVault_AcceptsRelativeInsideRoot(t *testing.T) {
	dir := t.TempDir()
	conn := &domain.Connection{Config: map[string]any{configVaultPath: dir}}

	got, err := ResolveInVault(conn, "notes/inbox.md")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks: %v", err)
	}
	want = filepath.Join(want, "notes", "inbox.md")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveInVault_RejectsEscape(t *testing.T) {
	dir := t.TempDir()
	conn := &domain.Connection{Config: map[string]any{configVaultPath: dir}}

	if _, err := ResolveInVault(conn, "../escape.md"); !errors.Is(err, tools.ErrPathEscapesRoot) {
		t.Fatalf("got %v, want ErrPathEscapesRoot", err)
	}
}

func TestResolveInVault_RejectsAbsoluteOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	conn := &domain.Connection{Config: map[string]any{configVaultPath: dir}}

	if _, err := ResolveInVault(conn, "/etc/passwd"); !errors.Is(err, tools.ErrPathEscapesRoot) {
		t.Fatalf("got %v, want ErrPathEscapesRoot", err)
	}
}

func TestResolveInVault_ConnectionWithoutVaultPath(t *testing.T) {
	conn := &domain.Connection{Config: map[string]any{}}
	if _, err := ResolveInVault(conn, "anything.md"); !errors.Is(err, ErrVaultPathMissing) {
		t.Fatalf("got %v, want ErrVaultPathMissing", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
