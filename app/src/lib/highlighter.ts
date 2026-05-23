// Lazy-loaded Shiki singleton. The grammar bundle is ~700KB; deferring
// the import to first use keeps it out of the initial Vite chunk. Once
// initialised, the highlighter is reused for every call until the user
// closes the renderer.
//
// Languages bundled cover the workflows the wedge actually serves
// (coding-agent on Go / TS / JS / Python / Rust + ops on bash + config
// in json / yaml). Anything else falls through to plaintext, which is
// the same UX the pre-Shiki path had.

import type { HighlighterCore } from "shiki";

let highlighterPromise: Promise<HighlighterCore | null> | null = null;

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

// Picks the dark/light theme variant Shiki should render with. Today
// we pin both to a single GitHub-flavoured pair so chat bubbles and
// diff hunks render the same; future iteration can switch on the
// user's chosen theme.
const LIGHT_THEME = "github-light";
const DARK_THEME = "github-dark";

async function ensureHighlighter(): Promise<HighlighterCore | null> {
  if (!highlighterPromise) {
    highlighterPromise = (async () => {
      try {
        const { createHighlighterCore, createOnigurumaEngine } = await import("shiki");
        const langModules = await Promise.all(
          BUNDLED_LANGS.map((l) => import(`shiki/langs/${l}.mjs`).then((m) => m.default)),
        );
        const lightTheme = (await import(`shiki/themes/${LIGHT_THEME}.mjs`)).default;
        const darkTheme = (await import(`shiki/themes/${DARK_THEME}.mjs`)).default;
        return await createHighlighterCore({
          themes: [lightTheme, darkTheme],
          langs: langModules,
          engine: createOnigurumaEngine(import("shiki/wasm")),
        });
      } catch (err) {
        // Highlighter init shouldn't fail the page; log + return null
        // so callers fall back to plain rendering.
        console.error("shiki: init failed:", err);
        return null;
      }
    })();
  }
  return highlighterPromise;
}

// normalizeLang maps loose lang strings (TypeScript, "TS", "javascript")
// down to the bundle names Shiki recognises. Returns null when no
// match exists so the caller can short-circuit to plain text.
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

// langFromPath sniffs a filename for the language hint. Used by
// DiffPreview where the diff header carries `+++ b/path/to/file.go`.
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

// highlightToHTML returns a Shiki-rendered <pre><code>…</code></pre>
// string for `code` in `lang`, or null if Shiki isn't ready yet or
// the language isn't bundled. Callers should treat null as "render the
// raw code unchanged" — never block the UI on highlighter readiness.
export async function highlightToHTML(
  code: string,
  lang: BundledLang | null,
): Promise<string | null> {
  if (!lang) return null;
  const h = await ensureHighlighter();
  if (!h) return null;
  try {
    return h.codeToHtml(code, {
      lang,
      themes: { light: LIGHT_THEME, dark: DARK_THEME },
      defaultColor: false, // emit CSS vars so the page picks light/dark
    });
  } catch {
    return null;
  }
}

// highlightLines tokenises `code` as `lang` and returns one HTML
// string per source line — the inner contents of each Shiki
// `<span class="line">` wrapper. The caller renders the per-line
// chrome (+/- gutter, bg tint, line numbers); this just gives them
// the syntax-coloured spans.
//
// One Shiki call per hunk preserves multi-line context: a template
// literal that spans three lines stays one token-tree instead of
// being re-tokenised from scratch each line, which is what the
// per-line HighlightedCode path used to do.
//
// Returns null when the language isn't bundled or Shiki failed to
// initialise; callers should render the raw code in that case.
export async function highlightLines(
  code: string,
  lang: BundledLang | null,
): Promise<string[] | null> {
  if (!lang) return null;
  const h = await ensureHighlighter();
  if (!h) return null;
  try {
    const html = h.codeToHtml(code, {
      lang,
      themes: { light: LIGHT_THEME, dark: DARK_THEME },
      defaultColor: false,
    });
    // Shiki wraps each line in <span class="line">…</span>. Pull the
    // inner contents out; the markup of the wrappers themselves is
    // discarded since the DiffPreview is doing its own per-line
    // wrapping with marker + tint classes.
    const lines: string[] = [];
    const re = /<span class="line">([\s\S]*?)<\/span>(?:\n|$)/g;
    let m: RegExpExecArray | null;
    while ((m = re.exec(html)) !== null) {
      lines.push(m[1]);
    }
    // Some Shiki output paths drop the trailing newline + don't emit
    // an empty last line; if the source code ends with `\n`, push an
    // empty trailing entry so caller's per-line zip stays aligned.
    if (code.endsWith("\n") && lines.length > 0 && lines[lines.length - 1] !== "") {
      lines.push("");
    }
    // If the regex couldn't find any `.line` spans (Shiki layout
    // change, theme without line-wrapping), bail to plain text rather
    // than rendering raw markup we can't reason about.
    if (lines.length === 0) return null;
    return lines;
  } catch {
    return null;
  }
}

// Warm the highlighter eagerly in idle time so the first chat /
// diff render doesn't pay the cold-start cost. Safe to no-op when
// idleCallback isn't available (Safari, headless).
export function warmHighlighter(): void {
  const idle =
    typeof window !== "undefined" &&
    "requestIdleCallback" in window
      ? (window.requestIdleCallback as (cb: () => void) => void)
      : (cb: () => void) => setTimeout(cb, 1500);
  idle(() => {
    void ensureHighlighter();
  });
}
