import { useMemo } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

interface MarkdownRendererProps {
  content: string;
}

export function MarkdownRenderer({ content }: MarkdownRendererProps) {
  const components = useMemo(
    () => ({
      pre({ children, ...props }: React.ComponentProps<"pre">) {
        // Extract language from code child's className if present
        let lang = "";
        const child = Array.isArray(children) ? children[0] : children;
        if (child && typeof child === "object" && "props" in child) {
          const cls =
            (child as React.ReactElement<{ className?: string }>).props
              .className || "";
          lang = cls.replace("language-", "");
        }
        return (
          <div className="md-code-wrap">
            {lang && <div className="md-code-lang">{lang}</div>}
            <pre {...props}>{children}</pre>
          </div>
        );
      },
      a({ children, href, ...props }: React.ComponentProps<"a">) {
        return (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="md-link"
            {...props}
          >
            {children}
          </a>
        );
      },
    }),
    [],
  );

  return (
    <div className="md-content">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
        {content}
      </ReactMarkdown>
    </div>
  );
}
