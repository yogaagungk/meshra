import { Suspense, useEffect, useMemo, useState } from "react";
import { Check, Copy, WrapText } from "lucide-react";
import { languageFor } from "../codeHighlight";
import "../code-view.css";

type Props = {
  code: string;
  /** File path: picks the language and is shown in the header. */
  path?: string;
  language?: string;
  /** Color added and removed lines. */
  diff?: boolean;
  label: string;
};

const MAX_HIGHLIGHT = 200_000;
const loadHighlighter = () => import("../codeHighlight");

function Lines({ code, language, diff }: { code: string; language: string; diff: boolean }) {
  const [lines, setLines] = useState<string[] | null>(null);
  useEffect(() => {
    let cancelled = false;
    if (!language || code.length > MAX_HIGHLIGHT) {
      setLines(null);
      return;
    }
    void loadHighlighter().then((m) => {
      if (!cancelled) setLines(m.highlightLines(code, language));
    });
    return () => {
      cancelled = true;
    };
  }, [code, language]);
  const plain = useMemo(() => code.split("\n"), [code]);
  const count = lines?.length ?? plain.length;
  const rows = [];
  for (let i = 0; i < count; i++) {
    const text = plain[i] ?? "";
    const kind = diff ? (text.startsWith("+++") || text.startsWith("---") ? "meta" : text.startsWith("+") ? "add" : text.startsWith("-") ? "del" : text.startsWith("@@") ? "hunk" : "") : "";
    rows.push(
      lines ? (
        <span key={i} className="code-line" data-n={i + 1} data-diff={kind} dangerouslySetInnerHTML={{ __html: lines[i] || "&#8203;" }} />
      ) : (
        <span key={i} className="code-line" data-n={i + 1} data-diff={kind}>{text || "​"}</span>
      ),
    );
  }
  return <code className="code-lines">{rows}</code>;
}

/**
 * Read-only code view: syntax colors, line numbers, wrap and copy. Colors come
 * from theme tokens, so it follows light and dark. Large files stay plain.
 */
export function CodeView({ code, path = "", language, diff = false, label }: Props) {
  const lang = diff ? "diff" : language ?? languageFor(path);
  const [wrap, setWrap] = useState(false);
  const [copied, setCopied] = useState(false);
  const total = useMemo(() => code.split("\n").length, [code]);
  async function copy() {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1600);
    } catch {
      /* clipboard unavailable */
    }
  }
  return (
    <figure className="code-view" data-wrap={wrap}>
      <figcaption>
        <span className="code-lang">{lang || "text"}</span>
        <span className="code-meta">{total.toLocaleString()} {total === 1 ? "line" : "lines"}</span>
        <span className="code-spacer" />
        <button type="button" onClick={() => setWrap((v) => !v)} aria-pressed={wrap} title="Wrap long lines">
          <WrapText size={14} /> Wrap
        </button>
        <button type="button" onClick={() => void copy()} title="Copy to clipboard">
          {copied ? <Check size={14} /> : <Copy size={14} />} {copied ? "Copied" : "Copy"}
        </button>
      </figcaption>
      <pre tabIndex={0} aria-label={label}>
        <Suspense fallback={null}>
          <Lines code={code} language={lang} diff={diff} />
        </Suspense>
      </pre>
    </figure>
  );
}

