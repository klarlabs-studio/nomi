package main

import (
	"fmt"
	"strings"
)

// Diff hunk parse/rebuild — mirrors extensions/vscode/src/diff_hunks.ts
// and app DiffPreview. Skipped hunks are dropped; @@ line counts are
// left alone (nomid's git apply / 3-way fallback tolerates that).

type parsedHunk struct {
	lines   []string
	added   int
	removed int
}

type parsedFileBlock struct {
	preamble  []string
	hunks     []parsedHunk
	fileLabel string
}

type hunkListItem struct {
	key       string
	fileLabel string
	hunkIndex int
	added     int
	removed   int
	header    string
}

func hunkKey(fileLabel string, hunkIndex int) string {
	return fmt.Sprintf("%s#%d", fileLabel, hunkIndex)
}

func parseHeaderPath(line string) string {
	// "--- a/foo" / "+++ b/foo" / "--- /dev/null"
	rest := strings.TrimSpace(line[4:])
	if strings.HasPrefix(rest, "a/") || strings.HasPrefix(rest, "b/") {
		rest = rest[2:]
	}
	// Drop trailing timestamp if present (rare in our diffs).
	if i := strings.IndexAny(rest, "\t "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func parseDiffStructure(diff string) []parsedFileBlock {
	var files []parsedFileBlock
	var current *parsedFileBlock
	var pending *parsedHunk
	pendingOldPath := ""

	flushHunk := func() {
		if pending != nil && current != nil {
			current.hunks = append(current.hunks, *pending)
			pending = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if current != nil {
			files = append(files, *current)
			current = nil
		}
	}

	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "--- ") {
			pendingOldPath = parseHeaderPath(line)
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			newPath := parseHeaderPath(line)
			fileLabel := newPath
			if fileLabel == "/dev/null" || fileLabel == "" {
				fileLabel = pendingOldPath
			}
			flushFile()
			oldP := pendingOldPath
			if oldP == "" {
				oldP = "/dev/null"
			}
			newP := newPath
			if newP == "" {
				newP = "/dev/null"
			}
			current = &parsedFileBlock{
				preamble:  []string{"--- " + oldP, "+++ " + newP},
				hunks:     nil,
				fileLabel: fileLabel,
			}
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			flushHunk()
			pending = &parsedHunk{lines: []string{line}}
			continue
		}
		if pending != nil {
			pending.lines = append(pending.lines, line)
			if strings.HasPrefix(line, "+") {
				pending.added++
			} else if strings.HasPrefix(line, "-") {
				pending.removed++
			}
		}
	}
	flushFile()
	return files
}

func rebuildDiff(blocks []parsedFileBlock, skipped map[string]bool) string {
	var out []string
	for _, block := range blocks {
		var kept []parsedHunk
		for i, h := range block.hunks {
			if skipped[hunkKey(block.fileLabel, i)] {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue
		}
		out = append(out, block.preamble...)
		for _, h := range kept {
			out = append(out, h.lines...)
		}
	}
	return strings.Join(out, "\n")
}

func applySkippedHunks(diff string, skipped map[string]bool) string {
	return rebuildDiff(parseDiffStructure(diff), skipped)
}

func listHunks(diff string) []hunkListItem {
	var out []hunkListItem
	for _, block := range parseDiffStructure(diff) {
		for i, h := range block.hunks {
			header := "@@"
			if len(h.lines) > 0 {
				header = h.lines[0]
			}
			out = append(out, hunkListItem{
				key:       hunkKey(block.fileLabel, i),
				fileLabel: block.fileLabel,
				hunkIndex: i,
				added:     h.added,
				removed:   h.removed,
				header:    header,
			})
		}
	}
	return out
}

// applyHunkSkipsToPlan mutates patch step arguments.diff for skipped keys.
// skippedByStep maps step id → hunk keys to drop.
func applyHunkSkipsToPlan(plan *planPayload, skippedByStep map[string][]string) (changed bool) {
	if plan == nil || len(skippedByStep) == 0 {
		return false
	}
	for i := range plan.Steps {
		s := &plan.Steps[i]
		keys := skippedByStep[s.ID]
		if len(keys) == 0 {
			continue
		}
		if s.ExpectedTool != "filesystem.patch" || s.Arguments == nil {
			continue
		}
		diff, _ := s.Arguments["diff"].(string)
		if diff == "" {
			continue
		}
		skip := make(map[string]bool, len(keys))
		for _, k := range keys {
			skip[k] = true
		}
		next := applySkippedHunks(diff, skip)
		if next == diff {
			continue
		}
		args := make(map[string]any, len(s.Arguments))
		for k, v := range s.Arguments {
			args[k] = v
		}
		args["diff"] = next
		s.Arguments = args
		changed = true
	}
	return changed
}
