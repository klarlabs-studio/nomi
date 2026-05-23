import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { HighlightedCode } from "@/components/highlighted-code";

// MarkdownMessage renders an LLM's response inside a chat bubble.
// react-markdown + remark-gfm cover the surface most models emit:
// headings, bold, italic, links, lists, tables, blockquotes, fenced
// code blocks, inline code. We deliberately keep the renderer narrow
// — no raw HTML pass-through, no JSX injection — so a prompt-injected
// response can't break out of the bubble.
//
// Styling: the wrapper carries `prose` shape via Tailwind utility
// composition. Headings dial down a level visually (h1 → text-base
// font-semibold) so a model that yells in h1s doesn't blow the bubble
// layout. Code blocks scroll horizontally; tables get min-width to
// avoid squashing.
export function MarkdownMessage({
  content,
  className = "",
}: {
  content: string;
  className?: string;
}) {
  return (
    <div className={"markdown-message text-sm leading-relaxed " + className}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        // Disable raw HTML so anything between < > is treated as text,
        // not parsed. The default react-markdown behaviour, but
        // making the intent explicit keeps a future contributor from
        // accidentally enabling `rehype-raw` and inheriting an XSS
        // surface.
        skipHtml
        components={{
          h1: ({ children }) => (
            <h1 className="text-base font-semibold mt-2 mb-1">{children}</h1>
          ),
          h2: ({ children }) => (
            <h2 className="text-base font-semibold mt-2 mb-1">{children}</h2>
          ),
          h3: ({ children }) => (
            <h3 className="text-sm font-semibold mt-2 mb-1">{children}</h3>
          ),
          p: ({ children }) => <p className="my-1 whitespace-pre-wrap">{children}</p>,
          ul: ({ children }) => <ul className="list-disc pl-5 my-1 space-y-0.5">{children}</ul>,
          ol: ({ children }) => <ol className="list-decimal pl-5 my-1 space-y-0.5">{children}</ol>,
          li: ({ children }) => <li className="leading-snug">{children}</li>,
          a: ({ href, children }) => (
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              className="underline underline-offset-2 hover:opacity-80"
            >
              {children}
            </a>
          ),
          blockquote: ({ children }) => (
            <blockquote className="border-l-2 border-muted-foreground/40 pl-3 my-2 italic text-muted-foreground">
              {children}
            </blockquote>
          ),
          code: ({ className: cls, children, ...props }) => {
            // Inline code: render as a tinted span. Fenced code blocks
            // are routed through Shiki via the `pre` override below.
            const text = String(children);
            const isBlock = /\n/.test(text) || (cls && /language-/.test(cls));
            if (isBlock) {
              // Strip trailing newline that ReactMarkdown adds. lang
              // comes from the `language-xxx` class react-markdown
              // attaches to fenced blocks.
              const lang = cls?.replace(/^language-/, "") ?? null;
              return <HighlightedCode code={text.replace(/\n$/, "")} lang={lang} />;
            }
            return (
              <code
                className="rounded bg-muted px-1 py-0.5 font-mono text-xs"
                {...props}
              >
                {children}
              </code>
            );
          },
          // Pre is rendered as-is; the inner <code> override above
          // unwraps the fenced block into a HighlightedCode component.
          // Returning a fragment avoids the extra <pre> wrapper that
          // would otherwise double-pad the Shiki output.
          pre: ({ children }) => <>{children}</>,
          table: ({ children }) => (
            <div className="overflow-x-auto my-2">
              <table className="min-w-full border-collapse text-xs">{children}</table>
            </div>
          ),
          th: ({ children }) => (
            <th className="border-b border-border px-2 py-1 text-left font-medium">
              {children}
            </th>
          ),
          td: ({ children }) => (
            <td className="border-b border-border/60 px-2 py-1">{children}</td>
          ),
          hr: () => <hr className="my-2 border-border" />,
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
}
