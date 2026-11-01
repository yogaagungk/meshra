import { lazy, Suspense, useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Check, Copy } from "lucide-react";
import { Button } from "./components/ui/button";
import mermaidFrameUrl from "./mermaid-frame.ts?worker&url";
import "./markdown.css";

const frameUrl = mermaidFrameUrl.replace(/&/g, "&amp;").replace(/"/g, "&quot;");
const mermaidDocument = `<!doctype html><html><head>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'nonce-meshra-renderer' 'strict-dynamic'; style-src 'unsafe-inline'; img-src 'none'; connect-src 'none'; font-src 'none'; media-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'">
<style>body{margin:12px;color:#303846}svg{display:block;max-width:100%}</style>
</head><body><script type="module" nonce="meshra-renderer" src="${frameUrl}"></script></body></html>`;

function MermaidPreview({ code }: { code: string }) {
  const frame = useRef<HTMLIFrameElement>(null);
  const loaded = useRef(false);
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [height, setHeight] = useState(180);
  const [theme, setTheme] = useState(() => document.documentElement.dataset.theme === "dark" ? "dark" : "light");

  useEffect(() => {
    const observer = new MutationObserver(() => setTheme(document.documentElement.dataset.theme === "dark" ? "dark" : "light"));
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (loaded.current) frame.current?.contentWindow?.postMessage({ code, theme }, "*");
  }, [code, theme]);

  useEffect(() => {
    const timeout = window.setTimeout(() => setStatus((current) => current === "loading" ? "error" : current), 15000);
    function receive(event: MessageEvent) {
      if (event.source !== frame.current?.contentWindow) return;
      if (event.data?.type === "mermaid-rendered" && Number.isFinite(event.data.height)) {
        setHeight(Math.max(100, Math.min(1200, event.data.height)));
        setStatus("ready");
      } else if (event.data?.type === "mermaid-error") {
        setStatus("error");
      }
    }
    window.addEventListener("message", receive);
    return () => {
      window.clearTimeout(timeout);
      window.removeEventListener("message", receive);
    };
  }, []);

  return (
    <div className="mermaid-preview">
      {status === "loading" && <div className="mermaid-message">Rendering diagram…</div>}
      {status === "error" && <div className="mermaid-message" role="alert">Unable to render this diagram. Select Code to view its source.</div>}
      <iframe
        ref={frame}
        title="Mermaid diagram preview"
        sandbox="allow-scripts"
        srcDoc={mermaidDocument}
        style={{ height: status === "ready" ? height : 0 }}
        onLoad={() => {
          loaded.current = true;
          frame.current?.contentWindow?.postMessage({ code, theme }, "*");
        }}
      />
    </div>
  );
}

function Mermaid({ code }: { code: string }) {
  const [preview, setPreview] = useState(true);
  const [copied, setCopied] = useState(false);
  return (
    <div className="code-block">
      <div className="code-toolbar">
        <span>mermaid</span>
        <div className="mermaid-toolbar-actions">
          <Button variant="ghost" size="sm" aria-pressed={preview} onClick={() => setPreview(true)}>Preview</Button>
          <Button variant="ghost" size="sm" aria-pressed={!preview} onClick={() => setPreview(false)}>Code</Button>
          <Button variant="ghost" size="sm" aria-label="Copy diagram source" onClick={() => {
            navigator.clipboard.writeText(code).then(() => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            });
          }}>
            {copied ? <Check size={14} /> : <Copy size={14} />} {copied ? "Copied" : "Copy"}
          </Button>
        </div>
      </div>
      {preview ? <MermaidPreview key={code} code={code} /> : <pre><code>{code}</code></pre>}
    </div>
  );
}

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
  if (language?.toLowerCase() === "mermaid") return <Mermaid code={code} />;
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
          table: ({ node: _node, ...props }) => (
            <div className="markdown-table-wrap" role="region" aria-label="Table" tabIndex={0}>
              <table {...props} />
            </div>
          ),
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
