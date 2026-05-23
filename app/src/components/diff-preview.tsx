import { useEffect, useMemo, useState } from "react";
import { ChevronDown, ChevronRight, Eye, EyeOff, Columns2, Rows2 } from "lucide-react";
import { langFromPath, highlightLines, type BundledLang } from "@/lib/highlighter";

interface DiffPreviewProps {
  diff: string;
  // onDiffChange fires when the user toggles per-hunk skips. The
  // PlanReviewCard can use it to push a /plan/edit so the patch that
  // ultimately gets applied matches what the user reviewed. Optional
  // because the read-only audit log surface uses the same component.
  onDiffChange?: (newDiff: string) => void;
}

interface DiffSummary {
  files: string[];
  added: number;
  removed: number;
}

const HEADER_RE = /^(?:---|\+\+\+)\s+(?:[ab]\/)?(.+?)\s*$/;
const HUNK_HEADER_RE = /^@@ /;
const VIEW_MODE_KEY = "nomi.diffPreview.viewMode";

interface ParsedHunk {
  // Lines belonging to this hunk including the @@ header.
  lines: string[];
  added: number;
  removed: number;
}

interface ParsedFileBlock {
  // Lines that precede the first @@ — the `--- /+++` headers + any
  // pre-hunk metadata like `index ...`.
  preamble: string[];
  hunks: ParsedHunk[];
  fileLabel: string;
}

// parseDiffStructure walks the diff and groups it by file → hunks
// so the UI can render and toggle per hunk. Doesn't attempt to fix
// hunk line counts when one is skipped — git apply re-validates so
// a slightly mis-summed @@ count would just trigger the 3-way
// fallback in the patch tool.
function parseDiffStructure(diff: string): ParsedFileBlock[] {
  const files: ParsedFileBlock[] = [];
  let current: ParsedFileBlock | null = null;
  let pendingHunk: ParsedHunk | null = null;
  let pendingOldPath = "";

  for (const line of diff.split("\n")) {
    if (line.startsWith("--- ")) {
      const m = HEADER_RE.exec(line);
      pendingOldPath = m ? m[1] : "";
      continue;
    }
    if (line.startsWith("+++ ")) {
      const m = HEADER_RE.exec(line);
      const newPath = m ? m[1] : "";
      const fileLabel = newPath !== "/dev/null" ? newPath : pendingOldPath;
      // Close the previous file block before starting a new one.
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
      if (pendingHunk && current) {
        current.hunks.push(pendingHunk);
      }
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

// summarizeDiff is the simple +/- counter retained for the header
// badge and exposed for callers that don't want the full structured
// parse.
function summarizeDiff(diff: string): DiffSummary {
  const blocks = parseDiffStructure(diff);
  const files = blocks.map((b) => b.fileLabel).filter((f) => f !== "/dev/null");
  let added = 0;
  let removed = 0;
  for (const b of blocks) {
    for (const h of b.hunks) {
      added += h.added;
      removed += h.removed;
    }
  }
  return { files, added, removed };
}

// rebuildDiff stitches selected hunks back into a unified diff. Skipped
// hunks are dropped entirely; preambles are kept so git apply always
// sees a `--- / +++` pair.
function rebuildDiff(blocks: ParsedFileBlock[], skipped: Set<string>): string {
  const out: string[] = [];
  for (const block of blocks) {
    const keptHunks = block.hunks.filter((_, i) => !skipped.has(`${block.fileLabel}#${i}`));
    if (keptHunks.length === 0) continue;
    out.push(...block.preamble);
    for (const h of keptHunks) {
      out.push(...h.lines);
    }
  }
  return out.join("\n");
}

/**
 * DiffPreview renders a unified-diff string. Affordances:
 *
 *  - Per-hunk skip toggle. Clicking "Skip" on a hunk drops it from the
 *    diff payload (state-managed; emits onDiffChange so the parent can
 *    persist via /plan/edit).
 *  - Side-by-side toggle. When on, adds and removes render in two
 *    columns instead of one; preference persists in localStorage.
 *  - Shiki per-hunk syntax highlighting. Markers are stripped before
 *    tokenisation so multi-line context (template strings, JSX, block
 *    comments) survives; +/- gutter + bg tint are overlaid on the
 *    tokenised output. Falls back to plain text when the language
 *    isn't bundled or Shiki hasn't finished initialising.
 *  - Color-coded `@@` hunk headers, +/- lines.
 */
export function DiffPreview({ diff, onDiffChange }: DiffPreviewProps) {
  const [expanded, setExpanded] = useState(true);
  const [skipped, setSkipped] = useState<Set<string>>(new Set());
  const [viewMode, setViewMode] = useState<"unified" | "split">(() => {
    if (typeof window === "undefined") return "unified";
    return window.localStorage.getItem(VIEW_MODE_KEY) === "split" ? "split" : "unified";
  });

  const blocks = useMemo(() => parseDiffStructure(diff), [diff]);
  const effectiveDiff = useMemo(() => rebuildDiff(blocks, skipped), [blocks, skipped]);
  const summary = useMemo(() => summarizeDiff(effectiveDiff), [effectiveDiff]);

  // Notify parent when the kept-hunks set changes so /plan/edit can
  // be issued before approve. Guarded against unnecessary fires by
  // letting React's effect-deps machinery handle dedup.
  useEffect(() => {
    if (onDiffChange) onDiffChange(effectiveDiff);
  }, [effectiveDiff, onDiffChange]);

  useEffect(() => {
    if (typeof window !== "undefined") {
      window.localStorage.setItem(VIEW_MODE_KEY, viewMode);
    }
  }, [viewMode]);

  const toggleHunk = (key: string) => {
    setSkipped((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    <div className="mt-2 rounded border border-muted-foreground/20 bg-background overflow-hidden">
      <div className="flex items-center justify-between gap-2 px-2.5 py-1.5 text-[11px] font-mono bg-muted/30">
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="flex items-center gap-1 min-w-0 hover:bg-muted/60 rounded px-1"
          aria-expanded={expanded}
        >
          {expanded ? (
            <ChevronDown className="w-3 h-3 flex-shrink-0" />
          ) : (
            <ChevronRight className="w-3 h-3 flex-shrink-0" />
          )}
          <span className="truncate">
            {summary.files.length > 0 ? summary.files.join(", ") : "patch"}
          </span>
        </button>
        <div className="flex items-center gap-2 flex-shrink-0">
          <span className="text-emerald-600 dark:text-emerald-400">+{summary.added}</span>
          <span className="text-rose-600 dark:text-rose-400">−{summary.removed}</span>
          <button
            type="button"
            onClick={() => setViewMode((m) => (m === "unified" ? "split" : "unified"))}
            className="p-1 hover:bg-muted/60 rounded"
            aria-label={viewMode === "unified" ? "Switch to side-by-side" : "Switch to unified"}
            title={viewMode === "unified" ? "Side-by-side" : "Unified"}
          >
            {viewMode === "unified" ? <Columns2 className="w-3 h-3" /> : <Rows2 className="w-3 h-3" />}
          </button>
        </div>
      </div>

      {expanded && (
        <div className="p-2 space-y-3 max-h-96 overflow-y-auto">
          {blocks.map((block, bi) => {
            const fileLang = langFromPath(block.fileLabel);
            return (
            <div key={bi} className="space-y-1">
              <div className="text-[11px] text-muted-foreground font-mono">
                {block.fileLabel}
              </div>
              {block.hunks.map((hunk, hi) => {
                const key = `${block.fileLabel}#${hi}`;
                const isSkipped = skipped.has(key);
                return (
                  <div key={hi} className={`border rounded ${isSkipped ? "opacity-50" : ""}`}>
                    <div className="flex items-center justify-between bg-muted/20 px-2 py-0.5 text-[10px] font-mono">
                      <span className="text-blue-600 dark:text-blue-400 truncate">
                        {hunk.lines[0]}
                      </span>
                      <button
                        type="button"
                        onClick={() => toggleHunk(key)}
                        className="flex items-center gap-1 hover:bg-muted/60 rounded px-1"
                        title={isSkipped ? "Include this hunk" : "Skip this hunk"}
                      >
                        {isSkipped ? (
                          <>
                            <Eye className="w-3 h-3" />
                            Include
                          </>
                        ) : (
                          <>
                            <EyeOff className="w-3 h-3" />
                            Skip
                          </>
                        )}
                      </button>
                    </div>
                    <HunkBody hunk={hunk} viewMode={viewMode} fileLang={fileLang} />
                  </div>
                );
              })}
            </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function HunkBody({
  hunk,
  viewMode,
  fileLang,
}: {
  hunk: ParsedHunk;
  viewMode: "unified" | "split";
  fileLang: BundledLang | null;
}) {
  const lines = hunk.lines.slice(1); // drop the @@ header which renders separately
  // Highlight once per hunk so multi-line tokens (template strings,
  // JSX, block comments) survive across line boundaries. The +/-
  // gutter + bg tint render on top of the syntax-highlighted spans.
  const tokens = useHunkHighlight(lines, fileLang);

  if (viewMode === "unified") {
    return <HighlightedDiffHunk lines={lines} tokens={tokens} side="both" />;
  }
  return (
    <div className="grid grid-cols-2 gap-px bg-muted-foreground/10 text-[11px] font-mono leading-tight">
      <HighlightedDiffHunk lines={lines} tokens={tokens} side="removed" />
      <HighlightedDiffHunk lines={lines} tokens={tokens} side="added" />
    </div>
  );
}

// useHunkHighlight returns one HTML string per source line, with the
// +/- markers stripped before tokenisation. Returns null while Shiki
// is loading or for languages outside the bundle so the caller falls
// back to plain text. Memoised on the (lines, lang) pair so re-renders
// from per-hunk skip toggles don't re-tokenise.
function useHunkHighlight(lines: string[], lang: BundledLang | null) {
  const cleaned = useMemo(
    () =>
      lines
        .map((l) => {
          const c = l.charAt(0);
          return c === "+" || c === "-" || c === " " ? l.slice(1) : l;
        })
        .join("\n"),
    [lines],
  );
  const [out, setOut] = useState<string[] | null>(null);

  useEffect(() => {
    let cancelled = false;
    if (!lang) {
      setOut(null);
      return;
    }
    void highlightLines(cleaned, lang).then((res) => {
      if (!cancelled) setOut(res);
    });
    return () => {
      cancelled = true;
    };
  }, [cleaned, lang]);

  return out;
}

// HighlightedDiffHunk renders one column (or both) of a diff hunk
// using pre-tokenised per-line HTML from useHunkHighlight + a +/-
// gutter overlay. `side="both"` is the unified view; "added" /
// "removed" are the two halves of the split view (the opposite side's
// lines become blank spacers so line numbers stay aligned).
function HighlightedDiffHunk({
  lines,
  tokens,
  side,
}: {
  lines: string[];
  tokens: string[] | null;
  side: "both" | "added" | "removed";
}) {
  return (
    <pre
      className={
        "text-[11px] font-mono leading-tight p-2 overflow-x-auto m-0 shiki-host " +
        (side === "both" ? "" : "bg-background")
      }
    >
      {lines.map((line, i) => {
        const marker = line.charAt(0);
        const content = line.slice(1);
        if (side === "added" && marker === "-") {
          return (
            <span key={i} className="block opacity-30">
              {" "}
            </span>
          );
        }
        if (side === "removed" && marker === "+") {
          return (
            <span key={i} className="block opacity-30">
              {" "}
            </span>
          );
        }
        let className = "block";
        if (marker === "+") className += " bg-emerald-500/10 text-emerald-700 dark:text-emerald-300";
        else if (marker === "-") className += " bg-rose-500/10 text-rose-700 dark:text-rose-300";
        // tokens may be misaligned with lines if Shiki normalised
        // trailing whitespace differently — fall back to the raw
        // content for that line rather than rendering garbled HTML.
        const tokenHTML = tokens && i < tokens.length ? tokens[i] : null;
        return (
          <span key={i} className={className}>
            <span className="select-none opacity-60">{marker || " "}</span>
            {tokenHTML !== null ? (
              <span
                className="inline"
                dangerouslySetInnerHTML={{ __html: tokenHTML || "&nbsp;" }}
              />
            ) : (
              <span className="inline">{content || " "}</span>
            )}
          </span>
        );
      })}
    </pre>
  );
}
