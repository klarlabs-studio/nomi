package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/felixgeelhaar/nomi/internal/domain"
)

// FileListTool lists directory entries inside an assistant's workspace root.
// Returns a structured listing the planner can reason about and a pre-rendered
// human-readable tree the chat surface can show as the step output.
type FileListTool struct{}

// NewFileListTool creates a new FileListTool.
func NewFileListTool() *FileListTool {
	return &FileListTool{}
}

// Name returns the tool name.
func (t *FileListTool) Name() string {
	return "filesystem.list"
}

// Capability returns the required capability — same surface as filesystem.read
// since listing only reveals names/sizes/mtimes already on disk.
func (t *FileListTool) Capability() string {
	return "filesystem.read"
}

// Execute lists a directory. Honors the workspace_root sandbox like
// filesystem.read. Optional inputs:
//   - path: directory under workspace_root (defaults to ".")
//   - recursive: walk subdirectories (default false)
//   - max_entries: cap on total entries returned (default 200)
func (t *FileListTool) Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error) {
	rawPath, _ := input["path"].(string)
	if rawPath == "" {
		rawPath = "."
	}
	root, err := WorkspaceRootFromInput(input)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeMissingWorkspace,
			Title:   "No workspace folder",
			Message: "This assistant doesn't have a folder context attached. Add one in the assistant builder so Nomi knows which folder to list.",
			Action:  "Open Assistant Builder",
		}
	}
	resolved, err := ResolveWithinRoot(root, rawPath)
	if err != nil {
		return nil, &domain.UserError{
			Code:    domain.ErrCodePathOutsideRoot,
			Title:   "Path outside workspace",
			Message: "The folder path goes outside the attached workspace. Nomi can only list folders inside the workspace.",
		}
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the folder: %w", err)
	}
	if !info.IsDir() {
		return nil, &domain.UserError{
			Code:    domain.ErrCodeToolExecution,
			Title:   "Not a folder",
			Message: fmt.Sprintf("%s is a file, not a folder. Use filesystem.read to read it.", rawPath),
		}
	}

	recursive, _ := input["recursive"].(bool)
	maxEntries := 200
	if v, ok := input["max_entries"].(float64); ok && v > 0 {
		maxEntries = int(v)
	}

	entries, truncated, err := walkDir(resolved, recursive, maxEntries)
	if err != nil {
		return nil, fmt.Errorf("couldn't walk the folder: %w", err)
	}

	tree := renderTree(resolved, entries, truncated)

	return map[string]interface{}{
		"path":      resolved,
		"count":     len(entries),
		"truncated": truncated,
		"entries":   entries,
		// `output` is what the chat surface renders as the step's response
		// text; keep the structured `entries` for downstream planner steps.
		"output": tree,
	}, nil
}

type listEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`            // "file" | "dir"
	Size     int64  `json:"size,omitempty"`  // bytes (files only)
	Modified string `json:"modified"`        // RFC3339
	Depth    int    `json:"depth,omitempty"` // recursive listings only
}

func walkDir(root string, recursive bool, max int) ([]listEntry, bool, error) {
	var out []listEntry
	truncated := false

	if !recursive {
		ds, err := os.ReadDir(root)
		if err != nil {
			return nil, false, err
		}
		// Sort: directories first, then by name. Stable for screenshots.
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].IsDir() != ds[j].IsDir() {
				return ds[i].IsDir()
			}
			return ds[i].Name() < ds[j].Name()
		})
		for _, d := range ds {
			if len(out) >= max {
				truncated = true
				break
			}
			out = append(out, dirEntryToListEntry(root, d, 0))
		}
		return out, truncated, nil
	}

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		if len(out) >= max {
			truncated = true
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		depth := strings.Count(rel, string(os.PathSeparator))
		out = append(out, dirEntryToListEntry(root, d, depth))
		return nil
	})
	return out, truncated, err
}

func dirEntryToListEntry(root string, d os.DirEntry, depth int) listEntry {
	full := filepath.Join(root, d.Name())
	e := listEntry{
		Name:  d.Name(),
		Path:  full,
		Depth: depth,
	}
	if d.IsDir() {
		e.Type = "dir"
	} else {
		e.Type = "file"
	}
	if info, err := d.Info(); err == nil {
		if !d.IsDir() {
			e.Size = info.Size()
		}
		e.Modified = info.ModTime().UTC().Format(time.RFC3339)
	}
	return e
}

// renderTree produces a human-readable folder listing the chat can show as-is.
func renderTree(root string, entries []listEntry, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", root)
	for i, e := range entries {
		last := i == len(entries)-1
		prefix := "├── "
		if last {
			prefix = "└── "
		}
		// Indent by depth for recursive listings.
		for d := 0; d < e.Depth; d++ {
			b.WriteString("│   ")
		}
		b.WriteString(prefix)
		b.WriteString(e.Name)
		if e.Type == "dir" {
			b.WriteString("/")
		} else {
			fmt.Fprintf(&b, "  (%s)", humanSize(e.Size))
		}
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("… (truncated)\n")
	}
	return b.String()
}

func humanSize(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
	)
	switch {
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
