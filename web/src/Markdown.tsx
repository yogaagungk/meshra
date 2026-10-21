import { lazy, Suspense, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Check, Copy } from "lucide-react";
import { Button } from "./components/ui/button";
const Highlight = lazy(() => import("./highlight"));
function Code({
  className,
  children,
}: {
  className?: string;
  children?: React.ReactNode;
}) {
  const [copied, setCopied] = useState(false);
  const code = String(children).replace(/\n$/, "");
  const language = /language-(\w+)/.exec(className || "")?.[1];
  if (!language && !String(children).includes("\n"))
    return <code className="inline-code">{children}</code>;
  return (
    <div className="code-block">
      <div className="code-toolbar">
        <span>{language || "text"}</span>
        <Button
          variant="ghost"
          size="sm"
          aria-label="Copy code"
          onClick={() => {
            navigator.clipboard.writeText(code).then(() => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            });
          }}
        >
          {copied ? <Check size={14} /> : <Copy size={14} />}{" "}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre>
        <Suspense fallback={<code>{code}</code>}>
          <Highlight code={code} language={language} />
        </Suspense>
      </pre>
    </div>
  );
}
export function Markdown({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const long = text.length > 14000;
  const visible = long && !expanded ? text.slice(0, 14000) + "\n\n…" : text;
  return (
    <div className="markdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          code: Code,
          pre: ({ children }) => <>{children}</>,
          img: ({ alt }) => (
            <span>[Image: {alt || "external image omitted"}]</span>
          ),
          a: ({ href, children }) => (
            <a href={href} target="_blank" rel="noopener noreferrer">
              {children}
            </a>
          ),
        }}
      >
        {visible}
      </ReactMarkdown>
      {long && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setExpanded(!expanded)}
        >
          {expanded ? "Collapse response" : "Show full response"}
        </Button>
      )}
    </div>
  );
}
