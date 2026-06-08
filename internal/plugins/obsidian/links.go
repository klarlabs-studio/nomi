package obsidian

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// wikilinkRe matches Obsidian's `[[target]]` and `[[target|alias]]`
// syntax. The target may contain forward slashes (folder paths) and
// dots (extensions). Aliases are everything after the first `|` up to
// the closing `]]`.
//
// We deliberately do NOT match links across newlines or with embedded
// newlines, since Obsidian itself doesn't either.
var wikilinkRe = regexp.MustCompile(`\[\[([^\]\n|#]+)(?:#([^\]\n|]+))?(?:\|([^\]\n]+))?\]\]`)

func (p *Plugin) linkTools() []toolDef {
	return []toolDef{
		{
			name:       "obsidian.link_notes",
			capability: "filesystem.write",
			desc:       "Add a wikilink from one note to another. Inserts `[[target]]` (or `[[target|alias]]`) at the end of the source note's body. Inputs: connection_id, source, target, alias?, separator? (default \" \").",
			run:        p.linkNotes,
		},
	}
}

func (p *Plugin) linkNotes(_ context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	source := stringInput(input, "source")
	target := stringInput(input, "target")
	if source == "" || target == "" {
		return nil, fmt.Errorf("obsidian.link_notes: source and target are required")
	}
	alias := stringInput(input, "alias")
	separator := stringInput(input, "separator")
	if separator == "" {
		separator = " "
	}

	abs, err := ResolveInVault(conn, source)
	if err != nil {
		return nil, fmt.Errorf("obsidian.link_notes: %w", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("obsidian.link_notes: read source: %w", err)
	}

	fm, body := parseFrontmatter(string(raw))
	linkText := formatWikilink(target, alias)

	// Idempotency: if the exact same link text already exists in the
	// body, no-op rather than spamming duplicates. Agents will retry
	// on partial failures and we don't want a single retry to leave
	// `[[foo]] [[foo]]` behind.
	if strings.Contains(body, linkText) {
		return map[string]any{
			"path":      source,
			"target":    target,
			"link_text": linkText,
			"appended":  false,
		}, nil
	}

	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += separator + linkText

	out := assembleNote(fm, body)
	if err := os.WriteFile(abs, []byte(out), 0o600); err != nil {
		return nil, fmt.Errorf("obsidian.link_notes: write: %w", err)
	}
	return map[string]any{
		"path":      source,
		"target":    target,
		"link_text": linkText,
		"appended":  true,
	}, nil
}

// extractWikilinks pulls every `[[target]]` reference out of body and
// returns the raw target strings (with folder paths intact, without
// the alias). Order is source order; duplicates are preserved so a
// caller wanting unique-only can dedupe.
func extractWikilinks(body string) []string {
	matches := wikilinkRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		target := strings.TrimSpace(m[1])
		if target == "" {
			continue
		}
		out = append(out, target)
	}
	return out
}

func formatWikilink(target, alias string) string {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, ".md")
	if alias = strings.TrimSpace(alias); alias != "" {
		return fmt.Sprintf("[[%s|%s]]", target, alias)
	}
	return fmt.Sprintf("[[%s]]", target)
}
