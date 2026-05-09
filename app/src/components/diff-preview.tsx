import { useMemo, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";

interface DiffPreviewProps {
  diff: string;
}

interface DiffSummary {
  files: string[];
  added: number;
  removed: number;
}

const HEADER_RE = /^(?:---|\+\+\+)\s+(?:[ab]\/)?(.+?)\s*$/;

// summarizeDiff reproduces the server-side parser in TypeScript so the
// review surface can render +/- counts and a file list without making
// an extra round-trip. The runtime re-validates the same diff before
// applying it, so this is best-effort display only.
function summarizeDiff(diff: string): DiffSummary {
  const files = new Set<string>();
  let added = 0;
  let removed = 0;
  for (const line of diff.split("\n")) {
    const match = HEADER_RE.exec(line);
    if (match) {
      if (match[1] !== "/dev/null") {
        files.add(match[1]);
      }
      continue;
    }
    if (line.startsWith("+++") || line.startsWith("---")) {
      continue;
    }
    if (line.startsWith("+")) {
      added += 1;
    } else if (line.startsWith("-")) {
      removed += 1;
    }
  }
  return { files: [...files], added, removed };
}

/**
 * DiffPreview renders a unified-diff string with a summary header
 * (file list + +/- counts) and a collapsible body that color-codes
 * adds/removes. Used inside PlanReviewCard whenever a step's
 * expected_tool is filesystem.patch — the user reads the change
 * before approving so write capability stays scoped to one diff.
 */
export function DiffPreview({ diff }: DiffPreviewProps) {
  const [expanded, setExpanded] = useState(true);
  const summary = useMemo(() => summarizeDiff(diff), [diff]);

  return (
    <div className="mt-2 rounded border border-muted-foreground/20 bg-background overflow-hidden">
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        className="w-full flex items-center justify-between gap-2 px-2.5 py-1.5 text-[11px] font-mono bg-muted/30 hover:bg-muted/60 transition-colors"
        aria-expanded={expanded}
      >
        <span className="flex items-center gap-1 min-w-0">
          {expanded ? (
            <ChevronDown className="w-3 h-3 flex-shrink-0" />
          ) : (
            <ChevronRight className="w-3 h-3 flex-shrink-0" />
          )}
          <span className="truncate">
            {summary.files.length > 0
              ? summary.files.join(", ")
              : "patch"}
          </span>
        </span>
        <span className="flex items-center gap-2 flex-shrink-0">
          <span className="text-emerald-600 dark:text-emerald-400">+{summary.added}</span>
          <span className="text-rose-600 dark:text-rose-400">−{summary.removed}</span>
        </span>
      </button>
      {expanded && (
        <pre className="text-[11px] font-mono leading-tight p-2 overflow-x-auto max-h-64 m-0">
          {diff.split("\n").map((line, i) => {
            let className = "block";
            if (line.startsWith("+++") || line.startsWith("---")) {
              className += " text-muted-foreground";
            } else if (line.startsWith("@@")) {
              className += " text-blue-600 dark:text-blue-400";
            } else if (line.startsWith("+")) {
              className += " text-emerald-600 dark:text-emerald-400";
            } else if (line.startsWith("-")) {
              className += " text-rose-600 dark:text-rose-400";
            }
            return (
              <span key={i} className={className}>
                {line || " "}
              </span>
            );
          })}
        </pre>
      )}
    </div>
  );
}
