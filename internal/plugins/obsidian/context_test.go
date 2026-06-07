package obsidian

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.klarlabs.de/nomi/internal/plugins"
)

func TestVaultContextSource_QueryReturnsScoredMatches(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "rust-perf.md"), `---
tags: [rust, performance]
---

Notes about rust borrow checker and arena allocation.
`)
	mustWrite(t, filepath.Join(dir, "memory-management.md"), `---
tags: [systems]
---

Memory management strategies.
`)
	mustWrite(t, filepath.Join(dir, "shopping.md"), "Bread and milk.\n")

	src := &vaultContextSource{
		connectionID: "c-test",
		vaultPath:    dir,
		displayName:  "Research Vault",
	}
	out, err := src.Query(context.Background(), plugins.ContextQueryRequest{Goal: "rust performance arena"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "Research Vault") {
		t.Fatalf("output should reference display name, got: %q", out)
	}
	if !strings.Contains(out, "rust-perf.md") {
		t.Fatalf("expected top hit rust-perf.md, got: %q", out)
	}
	if strings.Contains(out, "shopping.md") {
		t.Fatalf("unrelated note shopping.md should not appear, got: %q", out)
	}
	if !strings.Contains(out, "tags: rust, performance") {
		t.Fatalf("tags line missing for top hit: %q", out)
	}
}

func TestVaultContextSource_FollowsWikilinks(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "topic.md"), `---
tags: [research]
---

Notes on the research topic. See [[appendix]] for the source list.
`)
	// appendix.md is intentionally written so it does NOT match the
	// query directly — it should surface only via the wikilink graph.
	mustWrite(t, filepath.Join(dir, "appendix.md"), "Bibliography entries.\n")

	src := &vaultContextSource{vaultPath: dir, connectionID: "c"}
	out, err := src.Query(context.Background(), plugins.ContextQueryRequest{Goal: "research topic notes"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "## Linked notes (1-hop)") {
		t.Fatalf("expected linked notes section, got: %q", out)
	}
	if !strings.Contains(out, "appendix.md") {
		t.Fatalf("linked note appendix.md missing: %q", out)
	}
}

func TestVaultContextSource_RespectsObsidianIgnore(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".obsidianignore"), `# block templates and tmp drafts
Templates/
*.tmp.md
private/**
`)
	mustWrite(t, filepath.Join(dir, "Templates", "weekly.md"), "rust performance template")
	mustWrite(t, filepath.Join(dir, "draft.tmp.md"), "rust performance draft")
	mustWrite(t, filepath.Join(dir, "private", "secret.md"), "rust performance secret")
	mustWrite(t, filepath.Join(dir, "real.md"), "rust performance real notes")

	src := &vaultContextSource{vaultPath: dir, connectionID: "c"}
	out, err := src.Query(context.Background(), plugins.ContextQueryRequest{Goal: "rust performance"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "real.md") {
		t.Fatalf("non-ignored note should appear: %q", out)
	}
	for _, blocked := range []string{"Templates/weekly.md", "draft.tmp.md", "private/secret.md"} {
		if strings.Contains(out, blocked) {
			t.Fatalf("ignored path %s leaked into output: %q", blocked, out)
		}
	}
}

func TestVaultContextSource_NoMatchesProducesPlaceholder(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "n.md"), "irrelevant content")

	src := &vaultContextSource{vaultPath: dir, connectionID: "c"}
	out, err := src.Query(context.Background(), plugins.ContextQueryRequest{Goal: "nothing here matches xyz123"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out, "no notes matched") {
		t.Fatalf("expected placeholder, got: %q", out)
	}
}

func TestVaultContextSource_MissingVaultErrors(t *testing.T) {
	src := &vaultContextSource{vaultPath: filepath.Join(t.TempDir(), "does-not-exist"), connectionID: "c"}
	if _, err := src.Query(context.Background(), plugins.ContextQueryRequest{Goal: "anything"}); err == nil {
		t.Fatal("expected error for missing vault")
	}
}

func TestVaultContextSource_OutputIsCapped(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("rust performance ", 5000)
	for i := 0; i < 10; i++ {
		mustWrite(t, filepath.Join(dir, "n"+string(rune('0'+i))+".md"), long)
	}

	src := &vaultContextSource{vaultPath: dir, connectionID: "c"}
	out, err := src.Query(context.Background(), plugins.ContextQueryRequest{Goal: "rust performance"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(out) > contextOutputMaxBytes+200 {
		t.Fatalf("output should be capped near %d bytes, got %d", contextOutputMaxBytes, len(out))
	}
}

func TestTokenizeGoal(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"the and", nil}, // all stopwords
		{"Rust performance arena", []string{"rust", "performance", "arena"}},
		{"How should I use [[wikilinks]] for research?", []string{"use", "wikilinks", "research"}},
		{"a b cd", nil},                      // all under min length
		{"rust rust rust", []string{"rust"}}, // dedup
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := tokenizeGoal(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPluginContextSourcesNoConnections(t *testing.T) {
	p := &Plugin{}
	if got := p.ContextSources(); got != nil {
		t.Fatalf("expected nil with no connection repo, got %v", got)
	}
}
