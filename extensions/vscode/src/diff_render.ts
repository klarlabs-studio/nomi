// Host-rendered DiffPreview chrome for the Plan Review webview.
// Produces unified + side-by-side HTML; the webview toggles view mode
// via CSS (no second highlight pass).

import { parseDiffStructure, type ParsedHunk } from "./diff_hunks";
import { highlightLines, langFromPath, type BundledLang } from "./highlighter";

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function stripMarker(line: string): string {
  const c = line.charAt(0);
  return c === "+" || c === "-" || c === " " ? line.slice(1) : line;
}

async function tokensForHunk(
  hunk: ParsedHunk,
  lang: BundledLang | null,
  preferDark: boolean,
): Promise<string[] | null> {
  const body = hunk.lines.slice(1);
  if (body.length === 0) return null;
  const cleaned = body.map(stripMarker).join("\n");
  return highlightLines(cleaned, lang, preferDark);
}

function lineRow(
  line: string,
  tokenHTML: string | null | undefined,
  side: "both" | "added" | "removed",
): string {
  const marker = line.charAt(0);
  const content = stripMarker(line);
  if (side === "added" && marker === "-") {
    return `<span class="diff-line spacer">&nbsp;</span>`;
  }
  if (side === "removed" && marker === "+") {
    return `<span class="diff-line spacer">&nbsp;</span>`;
  }
  let cls = "diff-line";
  if (marker === "+") cls += " add";
  else if (marker === "-") cls += " rem";
  const gutter = escapeHtml(marker || " ");
  const body =
    tokenHTML != null
      ? `<span class="tok">${tokenHTML || "&nbsp;"}</span>`
      : `<span class="tok">${escapeHtml(content) || "&nbsp;"}</span>`;
  return `<span class="${cls}"><span class="gutter">${gutter}</span>${body}</span>`;
}

function renderHunkColumn(
  lines: string[],
  tokens: string[] | null,
  side: "both" | "added" | "removed",
): string {
  const rows = lines
    .map((line, i) => lineRow(line, tokens && i < tokens.length ? tokens[i] : null, side))
    .join("");
  return `<pre class="diff-pre shiki-host">${rows}</pre>`;
}

async function renderHunkBody(
  hunk: ParsedHunk,
  lang: BundledLang | null,
  preferDark: boolean,
): Promise<string> {
  const lines = hunk.lines.slice(1);
  const tokens = await tokensForHunk(hunk, lang, preferDark);
  const unified = renderHunkColumn(lines, tokens, "both");
  const rem = renderHunkColumn(lines, tokens, "removed");
  const add = renderHunkColumn(lines, tokens, "added");
  return `<div class="hunk-views">
    <div class="view-unified">${unified}</div>
    <div class="view-split"><div class="split-col">${rem}</div><div class="split-col">${add}</div></div>
  </div>`;
}

/**
 * Full DiffPreview HTML for one unified-diff string: per-file / per-hunk
 * chrome with Shiki tokens (when available) and dual unified/split markup.
 */
export async function renderDiffPreviewHtml(
  diff: string,
  preferDark: boolean,
): Promise<string> {
  const blocks = parseDiffStructure(diff);
  if (blocks.length === 0) {
    return `<pre class="diff-pre plain">${escapeHtml(diff)}</pre>`;
  }
  let added = 0;
  let removed = 0;
  const files: string[] = [];
  for (const b of blocks) {
    if (b.fileLabel && b.fileLabel !== "/dev/null") files.push(b.fileLabel);
    for (const h of b.hunks) {
      added += h.added;
      removed += h.removed;
    }
  }
  const fileBits: string[] = [];
  for (const block of blocks) {
    const lang = langFromPath(block.fileLabel);
    const hunkBits: string[] = [];
    for (let hi = 0; hi < block.hunks.length; hi++) {
      const hunk = block.hunks[hi]!;
      const header = escapeHtml(hunk.lines[0] ?? "@@");
      const body = await renderHunkBody(hunk, lang, preferDark);
      hunkBits.push(`<div class="hunk-block">
        <div class="hunk-hdr"><code>${header}</code> <span class="pm">+${hunk.added} −${hunk.removed}</span></div>
        ${body}
      </div>`);
    }
    fileBits.push(`<div class="file-block">
      <button type="button" class="file-label" data-open-path="${escapeHtml(block.fileLabel)}" title="Open in editor">${escapeHtml(block.fileLabel)}</button>
      ${hunkBits.join("")}
    </div>`);
  }
  const fileLinks = files
    .map(
      (f) =>
        `<button type="button" class="file-chip" data-open-path="${escapeHtml(f)}" title="Open in editor">${escapeHtml(f)}</button>`,
    )
    .join(" ");
  return `<div class="diff-preview">
    <div class="diff-summary">
      <span class="files">${fileLinks || escapeHtml("patch")}</span>
      <span class="pm add">+${added}</span>
      <span class="pm rem">−${removed}</span>
    </div>
    ${fileBits.join("")}
  </div>`;
}

/** CSS shared by Plan Review DiffPreview (unified + split + Shiki). */
export const DIFF_PREVIEW_CSS = `
.diff-preview {
  margin: 6px 0 0 22px;
  border: 1px solid var(--vscode-widget-border, #444);
  border-radius: 4px;
  overflow: hidden;
  font-size: 11px;
}
.diff-summary {
  display: flex;
  gap: 8px;
  align-items: center;
  padding: 4px 8px;
  background: var(--vscode-editorWidget-background, transparent);
  font-family: var(--vscode-editor-font-family, monospace);
}
.diff-summary .files { flex: 1; min-width: 0; overflow: hidden; display: flex; flex-wrap: wrap; gap: 4px; }
.file-label, .file-chip {
  appearance: none;
  background: transparent;
  border: none;
  padding: 0;
  margin: 0;
  color: var(--vscode-textLink-foreground, #4daafc);
  cursor: pointer;
  font: inherit;
  font-family: var(--vscode-editor-font-family, monospace);
  text-align: left;
  text-decoration: underline;
  text-underline-offset: 2px;
}
.file-label:hover, .file-chip:hover { opacity: 0.85; }
.file-label {
  display: block;
  padding: 3px 8px;
  width: 100%;
  box-sizing: border-box;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.file-chip { font-size: 11px; }
.pm.add { color: var(--vscode-gitDecoration-addedResourceForeground, #3fb950); }
.pm.rem { color: var(--vscode-gitDecoration-deletedResourceForeground, #f85149); }
.file-block { border-top: 1px solid var(--vscode-widget-border, #444); }
.hunk-block { border-top: 1px solid var(--vscode-widget-border, #333); }
.hunk-hdr {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  padding: 2px 8px;
  background: var(--vscode-editorWidget-background, transparent);
  font-family: var(--vscode-editor-font-family, monospace);
  color: var(--vscode-textLink-foreground, #4daafc);
}
.hunk-hdr code { font-size: 10px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.diff-pre {
  margin: 0;
  padding: 4px 6px;
  white-space: pre;
  overflow-x: auto;
  font-family: var(--vscode-editor-font-family, monospace);
  font-size: var(--vscode-editor-font-size, 12px);
  line-height: 1.4;
  background: var(--vscode-editor-background);
}
.diff-line { display: block; }
.diff-line.add { background: color-mix(in srgb, var(--vscode-gitDecoration-addedResourceForeground, #3fb950) 12%, transparent); }
.diff-line.rem { background: color-mix(in srgb, var(--vscode-gitDecoration-deletedResourceForeground, #f85149) 12%, transparent); }
.diff-line.spacer { opacity: 0.25; min-height: 1.4em; }
.gutter { display: inline-block; width: 1.2em; opacity: 0.55; user-select: none; }
.tok { display: inline; }
.view-split { display: none; grid-template-columns: 1fr 1fr; gap: 1px; background: var(--vscode-widget-border, #444); }
.split-col { background: var(--vscode-editor-background); min-width: 0; }
body.diff-split .view-unified { display: none; }
body.diff-split .view-split { display: grid; }
`;
