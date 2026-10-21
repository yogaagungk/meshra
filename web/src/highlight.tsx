import hljs from "highlight.js/lib/core";
import javascript from "highlight.js/lib/languages/javascript";
import typescript from "highlight.js/lib/languages/typescript";
import go from "highlight.js/lib/languages/go";
import json from "highlight.js/lib/languages/json";
import bash from "highlight.js/lib/languages/bash";
import python from "highlight.js/lib/languages/python";
import { useMemo } from "react";
for (const [name, grammar] of Object.entries({
  javascript,
  typescript,
  go,
  json,
  bash,
  python,
}))
  hljs.registerLanguage(name, grammar);
// Only highlight.js's escaped output enters this trusted rendering boundary.
export default function Highlight({
  code,
  language,
}: {
  code: string;
  language?: string;
}) {
  const html = useMemo(
    () =>
      language && hljs.getLanguage(language) && code.length < 30000
        ? hljs.highlight(code, { language, ignoreIllegals: true }).value
        : null,
    [code, language],
  );
  return html ? (
    <code dangerouslySetInnerHTML={{ __html: html }} />
  ) : (
    <code>{code}</code>
  );
}
