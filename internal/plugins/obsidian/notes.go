package obsidian

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// noteTools constructs the slice of obsidian.note.* tools the plugin
// contributes. read/create/update form the minimum CRUD surface a
// research or writing-partner assistant needs to be useful inside a
// vault.
func (p *Plugin) noteTools() []toolDef {
	return []toolDef{
		{
			name:       "obsidian.read_note",
			capability: "filesystem.read",
			desc:       "Read a single note by relative path. Returns content, parsed frontmatter, tags, and wikilinks. Inputs: connection_id, path.",
			run:        p.noteRead,
		},
		{
			name:       "obsidian.create_note",
			capability: "filesystem.write",
			desc:       "Create a new note. Refuses to overwrite an existing file. Optional frontmatter and tags are emitted as a YAML block above the body. Inputs: connection_id, path, content?, frontmatter?, tags?.",
			run:        p.noteCreate,
		},
		{
			name:       "obsidian.update_note",
			capability: "filesystem.write",
			desc:       "Update an existing note. Modes: replace (default) overwrites the body; append (append=true) adds to the end preserving any frontmatter. Optional frontmatter merges into the existing block. Inputs: connection_id, path, content, append?, frontmatter?, tags?.",
			run:        p.noteUpdate,
		},
	}
}

func (p *Plugin) noteRead(_ context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	rel := stringInput(input, "path")
	if rel == "" {
		return nil, fmt.Errorf("obsidian.read_note: path is required")
	}
	abs, err := ResolveInVault(conn, rel)
	if err != nil {
		return nil, fmt.Errorf("obsidian.read_note: %w", err)
	}
	raw, err := os.ReadFile(abs) //nolint:gosec // G304: path resolved within the vault root (ResolveInVault)
	if err != nil {
		return nil, fmt.Errorf("obsidian.read_note: read %s: %w", rel, err)
	}
	frontmatter, body := parseFrontmatter(string(raw))
	tags := tagsFromFrontmatter(frontmatter)
	return map[string]any{
		"path":        rel,
		"content":     string(raw),
		"body":        body,
		"frontmatter": frontmatter,
		"tags":        tags,
		"wikilinks":   extractWikilinks(body),
	}, nil
}

func (p *Plugin) noteCreate(_ context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	rel := stringInput(input, "path")
	if rel == "" {
		return nil, fmt.Errorf("obsidian.create_note: path is required")
	}
	if !strings.EqualFold(filepath.Ext(rel), ".md") {
		return nil, fmt.Errorf("obsidian.create_note: path must end in .md")
	}

	abs, err := ResolveInVault(conn, rel)
	if err != nil {
		return nil, fmt.Errorf("obsidian.create_note: %w", err)
	}

	body := stringInput(input, "content")
	frontmatter := mapInput(input, "frontmatter")
	tags := stringSliceInput(input, "tags")
	if len(tags) > 0 {
		if frontmatter == nil {
			frontmatter = map[string]any{}
		}
		frontmatter["tags"] = mergeStringSlice(tags, sliceFromAny(frontmatter["tags"]))
	}

	out := assembleNote(frontmatter, body)

	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("obsidian.create_note: mkdir parent: %w", err)
	}
	// O_EXCL refuses to overwrite. Agents update existing notes via
	// obsidian.update_note; create is "make a new one or fail loud".
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path resolved within the vault root (ResolveInVault)
	if err != nil {
		return nil, fmt.Errorf("obsidian.create_note: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(out); err != nil {
		return nil, fmt.Errorf("obsidian.create_note: write: %w", err)
	}
	return map[string]any{
		"path":          rel,
		"bytes_written": len(out),
	}, nil
}

func (p *Plugin) noteUpdate(_ context.Context, conn *domain.Connection, input map[string]any) (map[string]any, error) {
	rel := stringInput(input, "path")
	if rel == "" {
		return nil, fmt.Errorf("obsidian.update_note: path is required")
	}
	abs, err := ResolveInVault(conn, rel)
	if err != nil {
		return nil, fmt.Errorf("obsidian.update_note: %w", err)
	}
	existingBytes, err := os.ReadFile(abs) //nolint:gosec // G304: path resolved within the vault root (ResolveInVault)
	if err != nil {
		return nil, fmt.Errorf("obsidian.update_note: read existing: %w", err)
	}

	existingFM, existingBody := parseFrontmatter(string(existingBytes))
	mergeFM := mapInput(input, "frontmatter")
	tags := stringSliceInput(input, "tags")
	if len(tags) > 0 {
		if mergeFM == nil {
			mergeFM = map[string]any{}
		}
		mergeFM["tags"] = mergeStringSlice(sliceFromAny(existingFM["tags"]), tags)
	}
	mergedFM := mergeMaps(existingFM, mergeFM)

	newBody := existingBody
	contentRaw, hasContent := input["content"]
	contentStr, _ := contentRaw.(string)
	switch {
	case boolInput(input, "append", false):
		if hasContent && contentStr != "" {
			if newBody != "" && !strings.HasSuffix(newBody, "\n") {
				newBody += "\n"
			}
			newBody += contentStr
		}
	case hasContent:
		newBody = contentStr
	}

	out := assembleNote(mergedFM, newBody)
	if err := os.WriteFile(abs, []byte(out), 0o600); err != nil {
		return nil, fmt.Errorf("obsidian.update_note: write: %w", err)
	}
	return map[string]any{
		"path":          rel,
		"bytes_written": len(out),
	}, nil
}

// parseFrontmatter splits a markdown document into its frontmatter map
// (parsed best-effort) and the body. If the document has no leading
// `---` block, frontmatter is nil and body == content.
//
// This is not a full YAML parser. It handles the field shapes Obsidian
// users actually write: scalar `key: value`, inline arrays
// `tags: [a, b]`, and block-list arrays
//
//	tags:
//	  - a
//	  - b
//
// Anything more elaborate (nested maps, anchors, multi-line strings)
// round-trips as a string and is preserved on write only if the caller
// passes it back through.
func parseFrontmatter(content string) (map[string]any, string) {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) == 0 {
		return nil, content
	}
	if strings.TrimRight(lines[0], "\r\n") != "---" {
		return nil, content
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, content
	}

	out := map[string]any{}
	var (
		listKey    string
		listIndent int
	)
	for i := 1; i < end; i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		trimmed := strings.TrimSpace(line)

		if listKey != "" {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if strings.HasPrefix(trimmed, "- ") && indent > listIndent {
				val := cleanTag(strings.TrimPrefix(trimmed, "- "))
				if val != "" {
					out[listKey] = append(sliceFromAny(out[listKey]), val)
				}
				continue
			}
			listKey = ""
		}

		key, value, ok := splitFrontmatterField(line)
		if !ok {
			continue
		}
		switch {
		case value == "":
			listKey = key
			listIndent = len(line) - len(strings.TrimLeft(line, " \t"))
			out[key] = []string{}
		case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
			inner := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			items := []string{}
			for _, raw := range strings.Split(inner, ",") {
				if t := cleanTag(raw); t != "" {
					items = append(items, t)
				}
			}
			out[key] = items
		default:
			out[key] = strings.Trim(value, `"'`)
		}
	}

	bodyParts := lines[end+1:]
	body := strings.Join(bodyParts, "")
	return out, body
}

// assembleNote serializes a frontmatter map (if non-empty) followed by
// the body. Field order is: tags first (most semantically important),
// then remaining keys in alphabetical order for stable diffs.
func assembleNote(frontmatter map[string]any, body string) string {
	if len(frontmatter) == 0 {
		return body
	}
	var b strings.Builder
	b.WriteString("---\n")

	keys := make([]string, 0, len(frontmatter))
	hasTags := false
	for k := range frontmatter {
		if k == "tags" {
			hasTags = true
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if hasTags {
		writeFrontmatterField(&b, "tags", frontmatter["tags"])
	}
	for _, k := range keys {
		writeFrontmatterField(&b, k, frontmatter[k])
	}
	b.WriteString("---\n")
	if body != "" && !strings.HasPrefix(body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(body)
	return b.String()
}

func writeFrontmatterField(b *strings.Builder, key string, value any) {
	switch v := value.(type) {
	case []string:
		fmt.Fprintf(b, "%s: [%s]\n", key, joinQuoted(v))
	case []any:
		strs := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				strs = append(strs, s)
			}
		}
		fmt.Fprintf(b, "%s: [%s]\n", key, joinQuoted(strs))
	case string:
		fmt.Fprintf(b, "%s: %s\n", key, yamlScalar(v))
	default:
		fmt.Fprintf(b, "%s: %v\n", key, v)
	}
}

// yamlScalar quotes the value if it contains characters that would
// confuse our line-oriented parser on the round-trip — colons, leading
// hyphens, etc.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#\n") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "[") {
		return fmt.Sprintf(`"%s"`, strings.ReplaceAll(s, `"`, `\"`))
	}
	return s
}

func joinQuoted(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := make([]string, 0, len(items))
	for _, s := range items {
		// Quote items containing spaces or commas to keep inline-array
		// parsing unambiguous on the next read.
		if strings.ContainsAny(s, ", ") {
			out = append(out, fmt.Sprintf(`"%s"`, s))
		} else {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

func sliceFromAny(v any) []string {
	switch s := v.(type) {
	case []string:
		return append([]string(nil), s...)
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	case string:
		if s == "" {
			return nil
		}
		return []string{s}
	}
	return nil
}

func mergeStringSlice(a, b []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	if base == nil && overlay == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

func mapInput(input map[string]any, key string) map[string]any {
	if v, ok := input[key].(map[string]any); ok {
		return v
	}
	return nil
}

func tagsFromFrontmatter(frontmatter map[string]any) []string {
	if frontmatter == nil {
		return nil
	}
	out := sliceFromAny(frontmatter["tags"])
	if singular, ok := frontmatter["tag"].(string); ok && singular != "" {
		out = append(out, singular)
	}
	return out
}
