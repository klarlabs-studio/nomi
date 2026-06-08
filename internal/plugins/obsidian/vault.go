package obsidian

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/tools"
)

var (
	ErrConnectionRequired   = errors.New("obsidian: connection is required")
	ErrVaultPathMissing     = errors.New("obsidian: vault_path config is required")
	ErrVaultPathInvalidType = errors.New("obsidian: vault_path must be a string")
	ErrVaultNotADirectory   = errors.New("obsidian: vault path is not a directory")
)

// VaultMetadata is the snapshot returned by DiscoverVault. Used by the
// Connections UI to show "you've selected a folder with N notes" and by
// future tools to surface the tag list. NoteCount only counts top-level
// .md files plus markdown nested anywhere except hidden directories
// (Obsidian itself ignores dot-prefixed dirs by convention).
type VaultMetadata struct {
	Path              string   `json:"path"`
	HasObsidianConfig bool     `json:"has_obsidian_config"`
	NoteCount         int      `json:"note_count"`
	Tags              []string `json:"tags"`
}

// DiscoverVault walks the given path once and returns metadata: the
// presence of Obsidian's .obsidian config directory, the count of
// markdown notes, and the union of frontmatter tags across the vault.
//
// The walk skips hidden directories (anything starting with ".") so we
// don't crawl into .obsidian, .git, .trash, etc. The .obsidian
// directory presence is detected directly because we want the metadata
// signal even though we don't traverse it.
//
// Returns an error if the path doesn't exist or isn't a directory.
// Read errors on individual files are tolerated — a single unreadable
// note shouldn't block discovery, so they're skipped silently.
func DiscoverVault(path string) (VaultMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return VaultMetadata{}, fmt.Errorf("obsidian: stat vault path: %w", err)
	}
	if !info.IsDir() {
		return VaultMetadata{}, ErrVaultNotADirectory
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return VaultMetadata{}, fmt.Errorf("obsidian: resolve vault path: %w", err)
	}

	meta := VaultMetadata{Path: abs, Tags: []string{}}

	if cfg, err := os.Stat(filepath.Join(abs, ".obsidian")); err == nil && cfg.IsDir() {
		meta.HasObsidianConfig = true
	}

	tagSet := map[string]struct{}{}
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission/transient errors on a subtree shouldn't kill
			// discovery; skip the entry and continue.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == abs {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			return nil
		}
		meta.NoteCount++
		for _, tag := range scanFrontmatterTags(p) {
			tagSet[tag] = struct{}{}
		}
		return nil
	})
	if walkErr != nil {
		return VaultMetadata{}, fmt.Errorf("obsidian: walk vault: %w", walkErr)
	}

	tags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	meta.Tags = tags
	return meta, nil
}

// ResolveInVault is the single chokepoint every Obsidian tool calls
// before touching the filesystem. It pulls the vault path off the
// Connection and delegates to tools.ResolveWithinRoot, which handles
// symlink-aware sandboxing identically to the core filesystem tools.
//
// Callers pass relative paths the assistant requested ("notes/inbox.md");
// absolute paths are accepted only if they already resolve inside the
// vault root.
func ResolveInVault(conn *domain.Connection, rel string) (string, error) {
	root, err := VaultPathForConnection(conn)
	if err != nil {
		return "", err
	}
	return tools.ResolveWithinRoot(root, rel)
}

// scanFrontmatterTags reads the leading YAML frontmatter block of a
// markdown file (delimited by `---` lines) and returns the tags it
// declares. Supports the two shapes Obsidian itself accepts:
//
//	tags: [foo, bar, baz]            // inline list
//	tags:                            // block list
//	  - foo
//	  - bar
//
// Singular `tag:` is also tolerated. Tags are returned without the
// leading `#` prefix that Obsidian sometimes uses inline; the
// frontmatter form is conventionally bare.
//
// Errors (file gone, malformed YAML) return an empty slice rather than
// propagating — this is best-effort metadata for the wizard, not a
// validation pass.
func scanFrontmatterTags(path string) []string {
	f, err := os.Open(path) //nolint:gosec // G304: path produced by WalkDir over the vault root
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !scanner.Scan() {
		return nil
	}
	if strings.TrimSpace(scanner.Text()) != "---" {
		return nil
	}

	var (
		tags        []string
		inTagsBlock bool
		blockIndent int
	)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}

		if inTagsBlock {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if strings.HasPrefix(trimmed, "- ") && indent > blockIndent {
				if t := cleanTag(strings.TrimPrefix(trimmed, "- ")); t != "" {
					tags = append(tags, t)
				}
				continue
			}
			// Any non-list line at or under the parent indent ends the block.
			inTagsBlock = false
		}

		key, value, ok := splitFrontmatterField(line)
		if !ok {
			continue
		}
		if key != "tags" && key != "tag" {
			continue
		}

		switch {
		case value == "":
			inTagsBlock = true
			blockIndent = len(line) - len(strings.TrimLeft(line, " \t"))
		case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
			inner := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			for _, raw := range strings.Split(inner, ",") {
				if t := cleanTag(raw); t != "" {
					tags = append(tags, t)
				}
			}
		default:
			if t := cleanTag(value); t != "" {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// splitFrontmatterField splits a "key: value" frontmatter line. Returns
// ok=false for blank lines, comments, or list items (handled separately
// by the block-list scanner).
func splitFrontmatterField(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "- ") {
		return "", "", false
	}
	idx := strings.Index(trimmed, ":")
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(trimmed[:idx]), strings.TrimSpace(trimmed[idx+1:]), true
}

// cleanTag strips quotes, leading `#`, and surrounding whitespace from
// a single tag token. Empty results are filtered out by the caller.
func cleanTag(raw string) string {
	t := strings.TrimSpace(raw)
	t = strings.Trim(t, `"'`)
	t = strings.TrimPrefix(t, "#")
	return strings.TrimSpace(t)
}
