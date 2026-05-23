import { useEffect, useState } from "react";
import { highlightToHTML, normalizeLang, type BundledLang } from "@/lib/highlighter";

interface HighlightedCodeProps {
  code: string;
  lang?: string | BundledLang | null;
  className?: string;
  // forBlock=true wraps the result in a `<pre>` container with chat-
  // appropriate sizing; forBlock=false renders inline (no wrapper).
  // Defaults to true since most callers are fenced code blocks.
  forBlock?: boolean;
}

// HighlightedCode renders code with Shiki when available. While the
// highlighter is loading (first call) or the language isn't bundled,
// it shows the raw code in a styled <pre> so the UI is never empty.
// Once Shiki resolves, it swaps the highlighted HTML in. dangerously-
// setting innerHTML is safe here because the input is Shiki's own
// output, not user-generated content.
export function HighlightedCode({
  code,
  lang,
  className = "",
  forBlock = true,
}: HighlightedCodeProps) {
  const [html, setHtml] = useState<string | null>(null);
  const normalized = typeof lang === "string" ? normalizeLang(lang) : lang ?? null;

  useEffect(() => {
    let cancelled = false;
    if (!normalized) {
      setHtml(null);
      return;
    }
    void highlightToHTML(code, normalized).then((out) => {
      if (!cancelled) setHtml(out);
    });
    return () => {
      cancelled = true;
    };
  }, [code, normalized]);

  if (html) {
    return (
      <div
        className={`shiki-host ${forBlock ? "my-2" : ""} ${className}`}
        dangerouslySetInnerHTML={{ __html: html }}
      />
    );
  }
  // Fallback path while the highlighter is loading or for languages
  // not in the bundle. Keep the visual treatment matching the
  // post-highlight version so the swap-in is unobtrusive.
  return (
    <pre
      className={
        (forBlock
          ? "bg-muted rounded-md p-2 my-2 overflow-x-auto text-xs font-mono "
          : "font-mono text-xs ") + className
      }
    >
      <code>{code}</code>
    </pre>
  );
}
