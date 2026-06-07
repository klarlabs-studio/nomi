package obsidian

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/domain"
)

func newTestConn(t *testing.T) (*Plugin, *domain.Connection, string) {
	t.Helper()
	dir := t.TempDir()
	conn := &domain.Connection{
		ID:      "c-test",
		Enabled: true,
		Config:  map[string]any{configVaultPath: dir},
	}
	return &Plugin{}, conn, dir
}

func TestNoteRead_ReturnsContentFrontmatterAndLinks(t *testing.T) {
	p, conn, dir := newTestConn(t)
	mustWrite(t, filepath.Join(dir, "topic.md"), `---
title: Topic
tags: [research, ai]
---

This note links to [[other-note]] and [[notes/sub|Sub]].
`)

	out, err := p.noteRead(context.Background(), conn, map[string]any{"path": "topic.md"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := out["path"]; got != "topic.md" {
		t.Fatalf("path: %v", got)
	}
	body, _ := out["body"].(string)
	if !strings.Contains(body, "links to [[other-note]]") {
		t.Fatalf("body missing expected text: %q", body)
	}
	tags, _ := out["tags"].([]string)
	wantTags := []string{"research", "ai"}
	if !reflect.DeepEqual(tags, wantTags) {
		t.Fatalf("tags: got %v, want %v", tags, wantTags)
	}
	links, _ := out["wikilinks"].([]string)
	wantLinks := []string{"other-note", "notes/sub"}
	if !reflect.DeepEqual(links, wantLinks) {
		t.Fatalf("wikilinks: got %v, want %v", links, wantLinks)
	}
}

func TestNoteRead_RejectsMissingPath(t *testing.T) {
	p, conn, _ := newTestConn(t)
	if _, err := p.noteRead(context.Background(), conn, map[string]any{}); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestNoteRead_RejectsEscape(t *testing.T) {
	p, conn, _ := newTestConn(t)
	if _, err := p.noteRead(context.Background(), conn, map[string]any{"path": "../escape.md"}); err == nil {
		t.Fatal("expected sandbox escape to be refused")
	}
}

func TestNoteCreate_WritesNewNoteWithFrontmatter(t *testing.T) {
	p, conn, dir := newTestConn(t)

	out, err := p.noteCreate(context.Background(), conn, map[string]any{
		"path":    "inbox/today.md",
		"content": "Body of the note.\n",
		"tags":    []string{"daily", "inbox"},
		"frontmatter": map[string]any{
			"title": "Today",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := out["path"]; got != "inbox/today.md" {
		t.Fatalf("path: %v", got)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "inbox", "today.md"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, "---\n") {
		t.Fatalf("expected frontmatter prefix, got: %q", got)
	}
	if !strings.Contains(got, "tags: [daily, inbox]") {
		t.Fatalf("expected inline tags array, got: %q", got)
	}
	if !strings.Contains(got, "title: Today") {
		t.Fatalf("expected title field, got: %q", got)
	}
	if !strings.Contains(got, "Body of the note.") {
		t.Fatalf("expected body, got: %q", got)
	}
}

func TestNoteCreate_RefusesOverwrite(t *testing.T) {
	p, conn, dir := newTestConn(t)
	mustWrite(t, filepath.Join(dir, "exists.md"), "already here")

	_, err := p.noteCreate(context.Background(), conn, map[string]any{
		"path":    "exists.md",
		"content": "would overwrite",
	})
	if err == nil {
		t.Fatal("expected refusal to overwrite existing file")
	}
}

func TestNoteCreate_RejectsNonMarkdownExtension(t *testing.T) {
	p, conn, _ := newTestConn(t)
	_, err := p.noteCreate(context.Background(), conn, map[string]any{
		"path":    "secrets.txt",
		"content": "plain text",
	})
	if err == nil {
		t.Fatal("expected rejection of non-.md path")
	}
}

func TestNoteUpdate_ReplaceMode(t *testing.T) {
	p, conn, dir := newTestConn(t)
	mustWrite(t, filepath.Join(dir, "n.md"), `---
tags: [keep]
---

Old body.
`)

	if _, err := p.noteUpdate(context.Background(), conn, map[string]any{
		"path":    "n.md",
		"content": "Brand new body.\n",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "n.md"))
	got := string(raw)
	if strings.Contains(got, "Old body") {
		t.Fatalf("replace should have removed old body: %q", got)
	}
	if !strings.Contains(got, "Brand new body") {
		t.Fatalf("replace should have written new body: %q", got)
	}
	// Frontmatter must survive a replace.
	if !strings.Contains(got, "tags: [keep]") {
		t.Fatalf("frontmatter lost on replace: %q", got)
	}
}

func TestNoteUpdate_AppendMode(t *testing.T) {
	p, conn, dir := newTestConn(t)
	mustWrite(t, filepath.Join(dir, "log.md"), "Existing line.\n")

	if _, err := p.noteUpdate(context.Background(), conn, map[string]any{
		"path":    "log.md",
		"content": "Appended line.",
		"append":  true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "log.md"))
	got := string(raw)
	if !strings.Contains(got, "Existing line.") || !strings.Contains(got, "Appended line.") {
		t.Fatalf("append should preserve existing + add new: %q", got)
	}
}

func TestNoteUpdate_MergesFrontmatterTags(t *testing.T) {
	p, conn, dir := newTestConn(t)
	mustWrite(t, filepath.Join(dir, "n.md"), `---
tags: [a, b]
title: Hello
---

body
`)
	if _, err := p.noteUpdate(context.Background(), conn, map[string]any{
		"path": "n.md",
		"tags": []string{"b", "c"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "n.md"))
	got := string(raw)
	if !strings.Contains(got, "tags: [a, b, c]") {
		t.Fatalf("tags should merge dedup: %q", got)
	}
	if !strings.Contains(got, "title: Hello") {
		t.Fatalf("non-tag fields should be preserved: %q", got)
	}
}

func TestParseFrontmatter_RoundTripsBlockList(t *testing.T) {
	src := `---
tags:
  - alpha
  - beta
title: Round-trip
---

body line one
body line two
`
	fm, body := parseFrontmatter(src)
	tags := sliceFromAny(fm["tags"])
	wantTags := []string{"alpha", "beta"}
	if !reflect.DeepEqual(tags, wantTags) {
		t.Fatalf("tags: got %v, want %v", tags, wantTags)
	}
	if fm["title"] != "Round-trip" {
		t.Fatalf("title: %v", fm["title"])
	}
	if !strings.Contains(body, "body line one") {
		t.Fatalf("body lost: %q", body)
	}

	// Reassemble and re-parse: tags must survive.
	round := assembleNote(fm, body)
	fm2, _ := parseFrontmatter(round)
	if got := sliceFromAny(fm2["tags"]); !reflect.DeepEqual(got, wantTags) {
		t.Fatalf("round-trip tags: got %v, want %v", got, wantTags)
	}
}

func TestParseFrontmatter_NoneReturnsBodyUnchanged(t *testing.T) {
	src := "no frontmatter here\nsecond line\n"
	fm, body := parseFrontmatter(src)
	if fm != nil {
		t.Fatalf("expected nil frontmatter, got %v", fm)
	}
	if body != src {
		t.Fatalf("body should equal input verbatim, got %q", body)
	}
}
