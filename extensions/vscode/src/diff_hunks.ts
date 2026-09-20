// Pure unified-diff hunk parse/rebuild — mirrors
// app/src/components/diff-preview.tsx without React/Shiki.
// Skipped hunks are dropped; @@ line counts are left alone (nomid's
// git apply / 3-way fallback tolerates that, same as desktop).

const HEADER_RE = /^(?:---|\+\+\+)\s+(?:[ab]\/)?(.+?)\s*$/;
const HUNK_HEADER_RE = /^@@ /;

export type ParsedHunk = {
  lines: string[];
  added: number;
  removed: number;
};

export type ParsedFileBlock = {
  preamble: string[];
  hunks: ParsedHunk[];
  fileLabel: string;
};

export function hunkKey(fileLabel: string, hunkIndex: number): string {
  return `${fileLabel}#${hunkIndex}`;
}

export function parseDiffStructure(diff: string): ParsedFileBlock[] {
  const files: ParsedFileBlock[] = [];
  let current: ParsedFileBlock | null = null;
  let pendingHunk: ParsedHunk | null = null;
  let pendingOldPath = "";

  for (const line of diff.split("\n")) {
    if (line.startsWith("--- ")) {
      const m = HEADER_RE.exec(line);
      pendingOldPath = m?.[1] ?? "";
      continue;
    }
    if (line.startsWith("+++ ")) {
      const m = HEADER_RE.exec(line);
      const newPath = m?.[1] ?? "";
      const fileLabel = newPath !== "/dev/null" ? newPath : pendingOldPath;
      if (current) {
        if (pendingHunk) {
          current.hunks.push(pendingHunk);
          pendingHunk = null;
        }
        files.push(current);
      }
      current = {
        preamble: [`--- ${pendingOldPath || "/dev/null"}`, `+++ ${newPath || "/dev/null"}`],
        hunks: [],
        fileLabel,
      };
      continue;
    }
    if (HUNK_HEADER_RE.test(line)) {
      if (pendingHunk && current) current.hunks.push(pendingHunk);
      pendingHunk = { lines: [line], added: 0, removed: 0 };
      continue;
    }
    if (pendingHunk) {
      pendingHunk.lines.push(line);
      if (line.startsWith("+")) pendingHunk.added += 1;
      else if (line.startsWith("-")) pendingHunk.removed += 1;
    }
  }
  if (pendingHunk && current) current.hunks.push(pendingHunk);
  if (current) files.push(current);
  return files;
}

/** Drop skipped hunks; omit file blocks with zero kept hunks. */
export function rebuildDiff(blocks: ParsedFileBlock[], skipped: Set<string>): string {
  const out: string[] = [];
  for (const block of blocks) {
    const kept = block.hunks.filter((_, i) => !skipped.has(hunkKey(block.fileLabel, i)));
    if (kept.length === 0) continue;
    out.push(...block.preamble);
    for (const h of kept) out.push(...h.lines);
  }
  return out.join("\n");
}

export function applySkippedHunks(diff: string, skipped: Set<string>): string {
  return rebuildDiff(parseDiffStructure(diff), skipped);
}

export type HunkListItem = {
  key: string;
  fileLabel: string;
  hunkIndex: number;
  added: number;
  removed: number;
  header: string;
};

/** Flat list for webview checkboxes. */
export function listHunks(diff: string): HunkListItem[] {
  const out: HunkListItem[] = [];
  for (const block of parseDiffStructure(diff)) {
    block.hunks.forEach((h, i) => {
      out.push({
        key: hunkKey(block.fileLabel, i),
        fileLabel: block.fileLabel,
        hunkIndex: i,
        added: h.added,
        removed: h.removed,
        header: h.lines[0] ?? "@@",
      });
    });
  }
  return out;
}
