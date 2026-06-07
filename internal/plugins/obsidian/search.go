package obsidian

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

const defaultSearchLimit = 25

func (p *Plugin) searchTools() []toolDef {
	return []toolDef{
		{
			name:       "obsidian.search_notes",
			capability: "filesystem.read",
			desc:       "Search notes by text, tags, and link graph. All filters compose (AND between filters, OR within tags / links_to). Inputs: connection_id, query?, tags?, links_to?, limit? (default 25).",
			run:        p.searchNotes,
		},
	}
}

// searchNotes implements text + tag + link-graph search across the
// vault. Without filters, returns the most recently modified N notes.
//
// Scoring is simple and explicit: each substring match of `query`
// inside the body adds 1; a filename match adds 5. Ties broken by
// descending modtime so newer notes float up. The point isn't a great
// IR system — full-text search lands later if Mnemos-style embeddings
// arrive — it's a good-enough surface so a research assistant can find
// "the note about X" without a separate index.
func (p *Plugin) searchNotes(_ context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	root, err := VaultPathForConnection(conn)
	if err != nil {
		return nil, fmt.Errorf("obsidian.search_notes: %w", err)
	}
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("obsidian.search_notes: vault inaccessible: %w", err)
	}

	query := strings.ToLower(stringInput(input, "query"))
	tagFilter := stringSliceInput(input, "tags")
	linksTo := normalizeLinkTargets(stringSliceInput(input, "links_to"))
	limit := intInput(input, "limit", defaultSearchLimit)
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	type match struct {
		Path    string   `json:"path"`
		Score   float64  `json:"score"`
		Snippet string   `json:"snippet,omitempty"`
		Tags    []string `json:"tags,omitempty"`
		Links   []string `json:"links,omitempty"`
		ModTime int64    `json:"-"`
	}

	var matches []match

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		fm, body := parseFrontmatter(string(raw))
		tags := tagsFromFrontmatter(fm)
		links := extractWikilinks(body)
		linkTargets := normalizeLinkTargets(links)

		if !matchesTagFilter(tags, tagFilter) {
			return nil
		}
		if !matchesLinkFilter(linkTargets, linksTo) {
			return nil
		}

		score := 0.0
		var snippet string
		if query != "" {
			lowerBody := strings.ToLower(body)
			occurrences := strings.Count(lowerBody, query)
			if occurrences == 0 && !strings.Contains(strings.ToLower(d.Name()), query) {
				return nil
			}
			score = float64(occurrences)
			if strings.Contains(strings.ToLower(d.Name()), query) {
				score += 5
			}
			snippet = makeSnippet(body, query)
		} else {
			score = 1
		}

		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p
		}

		modTime := int64(0)
		if info, err := d.Info(); err == nil {
			modTime = info.ModTime().Unix()
		}

		matches = append(matches, match{
			Path:    rel,
			Score:   score,
			Snippet: snippet,
			Tags:    tags,
			Links:   links,
			ModTime: modTime,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("obsidian.search_notes: walk: %w", walkErr)
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].ModTime > matches[j].ModTime
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}

	out := make([]map[string]any, 0, len(matches))
	for _, m := range matches {
		out = append(out, map[string]any{
			"path":    m.Path,
			"score":   m.Score,
			"snippet": m.Snippet,
			"tags":    m.Tags,
			"links":   m.Links,
		})
	}
	return map[string]any{
		"matches": out,
		"count":   len(out),
	}, nil
}

func matchesTagFilter(noteTags, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	have := map[string]struct{}{}
	for _, t := range noteTags {
		have[strings.ToLower(t)] = struct{}{}
	}
	for _, f := range filter {
		if _, ok := have[strings.ToLower(f)]; ok {
			return true
		}
	}
	return false
}

func matchesLinkFilter(noteLinks, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	have := map[string]struct{}{}
	for _, l := range noteLinks {
		have[strings.ToLower(l)] = struct{}{}
	}
	for _, f := range filter {
		if _, ok := have[strings.ToLower(f)]; ok {
			return true
		}
	}
	return false
}

// normalizeLinkTargets strips .md suffixes and folder prefixes so that
// `[[notes/foo]]`, `[[notes/foo.md]]`, and `[[foo]]` all compare equal
// from a graph perspective. Obsidian itself resolves wikilinks by
// basename; we mirror that.
func normalizeLinkTargets(targets []string) []string {
	if len(targets) == 0 {
		return nil
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		t = strings.TrimSuffix(t, ".md")
		if idx := strings.LastIndex(t, "/"); idx >= 0 {
			t = t[idx+1:]
		}
		out = append(out, t)
	}
	return out
}

// makeSnippet returns ~80 chars of context around the first match of
// query in body, lowercase-comparing but preserving original casing in
// the returned slice. Empty string if query isn't present.
func makeSnippet(body, query string) string {
	const radius = 40
	lower := strings.ToLower(body)
	idx := strings.Index(lower, query)
	if idx < 0 {
		return ""
	}
	start := idx - radius
	if start < 0 {
		start = 0
	}
	end := idx + len(query) + radius
	if end > len(body) {
		end = len(body)
	}
	snippet := body[start:end]
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(body) {
		snippet = snippet + "…"
	}
	return snippet
}
