// Host-side Shiki singleton for Plan Review DiffPreview.
// Highlight in the extension host, inject HTML into the webview —
// webview CSP cannot load Shiki WASM. Falls back to plain text when
// the language isn't bundled or init fails.

import type { HighlighterGeneric } from "shiki";

const BUNDLED_LANGS = [
  "go",
  "typescript",
  "tsx",
  "javascript",
  "jsx",
  "python",
  "rust",
  "bash",
  "shell",
  "json",
  "yaml",
  "markdown",
  "sql",
  "diff",
] as const;

export type BundledLang = (typeof BUNDLED_LANGS)[number];

const LIGHT_THEME = "github-light";
const DARK_THEME = "github-dark";

type Highlighter = HighlighterGeneric<string, string>;

let highlighterPromise: Promise<Highlighter | null> | null = null;

async function ensureHighlighter(): Promise<Highlighter | null> {
  if (!highlighterPromise) {
    highlighterPromise = (async () => {
      try {
        const { createHighlighter } = await import("shiki");
        return (await createHighlighter({
          themes: [LIGHT_THEME, DARK_THEME],
          langs: [...BUNDLED_LANGS],
        })) as Highlighter;
      } catch (err) {
        console.error("nomi shiki: init failed:", err);
        return null;
      }
    })();
  }
  return highlighterPromise;
}

export function normalizeLang(input: string | undefined): BundledLang | null {
  if (!input) return null;
  const k = input.trim().toLowerCase();
  const map: Record<string, BundledLang> = {
    ts: "typescript",
    typescript: "typescript",
    tsx: "tsx",
    js: "javascript",
    javascript: "javascript",
    jsx: "jsx",
    go: "go",
    golang: "go",
    py: "python",
    python: "python",
    rs: "rust",
    rust: "rust",
    sh: "bash",
    bash: "bash",
    shell: "shell",
    zsh: "bash",
    json: "json",
    yaml: "yaml",
    yml: "yaml",
    md: "markdown",
    markdown: "markdown",
    sql: "sql",
    diff: "diff",
    patch: "diff",
  };
  return map[k] ?? null;
}

export function langFromPath(path: string): BundledLang | null {
  const ext = path.split(".").pop()?.toLowerCase();
  if (!ext) return null;
  const m: Record<string, BundledLang> = {
    go: "go",
    ts: "typescript",
    tsx: "tsx",
    js: "javascript",
    jsx: "jsx",
    py: "python",
    rs: "rust",
    sh: "bash",
    bash: "bash",
    zsh: "bash",
    json: "json",
    yaml: "yaml",
    yml: "yaml",
    md: "markdown",
    sql: "sql",
  };
  return m[ext] ?? null;
}

/**
 * One HTML fragment per source line (inner contents of Shiki `.line`
 * spans). Markers must already be stripped. Returns null when the
 * language isn't bundled or Shiki isn't ready.
 */
export async function highlightLines(
  code: string,
  lang: BundledLang | null,
  preferDark: boolean,
): Promise<string[] | null> {
  if (!lang) return null;
  const h = await ensureHighlighter();
  if (!h) return null;
  try {
    const theme = preferDark ? DARK_THEME : LIGHT_THEME;
    const html = h.codeToHtml(code, { lang, theme });
    const lines: string[] = [];
    // Shiki emits one `<span class="line">…</span>` per line, separated
    // by newlines — non-greedy match to the first `</span>` that is
    // followed by `\n` or end-of-string (skips nested token closes).
    const re = /<span class="line">([\s\S]*?)<\/span>(?:\n|$)/g;
    let m: RegExpExecArray | null;
    while ((m = re.exec(html)) !== null) {
      lines.push(m[1]);
    }
    if (code.endsWith("\n") && lines.length > 0 && lines[lines.length - 1] !== "") {
      lines.push("");
    }
    if (lines.length === 0) return null;
    return lines;
  } catch {
    return null;
  }
}

/** Warm on activate so the first Plan Review doesn't pay cold-start. */
export function warmHighlighter(): void {
  void ensureHighlighter();
}
