import hljs from "highlight.js/lib/core";
import bash from "highlight.js/lib/languages/bash";
import c from "highlight.js/lib/languages/c";
import cpp from "highlight.js/lib/languages/cpp";
import css from "highlight.js/lib/languages/css";
import diff from "highlight.js/lib/languages/diff";
import dockerfile from "highlight.js/lib/languages/dockerfile";
import go from "highlight.js/lib/languages/go";
import ini from "highlight.js/lib/languages/ini";
import java from "highlight.js/lib/languages/java";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import kotlin from "highlight.js/lib/languages/kotlin";
import makefile from "highlight.js/lib/languages/makefile";
import markdown from "highlight.js/lib/languages/markdown";
import php from "highlight.js/lib/languages/php";
import python from "highlight.js/lib/languages/python";
import ruby from "highlight.js/lib/languages/ruby";
import rust from "highlight.js/lib/languages/rust";
import scss from "highlight.js/lib/languages/scss";
import sql from "highlight.js/lib/languages/sql";
import swift from "highlight.js/lib/languages/swift";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";

const LANGUAGES = { bash, c, cpp, css, diff, dockerfile, go, ini, java, javascript, json, kotlin, makefile, markdown, php, python, ruby, rust, scss, sql, swift, typescript, xml, yaml };
for (const [name, grammar] of Object.entries(LANGUAGES)) {
  if (!hljs.getLanguage(name)) hljs.registerLanguage(name, grammar);
}

const BY_EXTENSION: Record<string, string> = {
  js: "javascript", mjs: "javascript", cjs: "javascript", jsx: "javascript",
  ts: "typescript", tsx: "typescript", mts: "typescript",
  json: "json", jsonc: "json", go: "go", py: "python", rb: "ruby", rs: "rust",
  sh: "bash", bash: "bash", zsh: "bash", env: "ini",
  yml: "yaml", yaml: "yaml", toml: "ini", ini: "ini", cfg: "ini", conf: "ini",
  css: "css", scss: "scss", html: "xml", htm: "xml", xml: "xml", svg: "xml", vue: "xml",
  md: "markdown", markdown: "markdown", sql: "sql", java: "java", kt: "kotlin",
  c: "c", h: "c", cc: "cpp", cpp: "cpp", hpp: "cpp", swift: "swift", php: "php",
  diff: "diff", patch: "diff", mk: "makefile",
};
const BY_NAME: Record<string, string> = { dockerfile: "dockerfile", makefile: "makefile", ".env": "ini", ".gitignore": "ini" };

/** Language id for a file path, or "" when it should stay plain text. */
export function languageFor(path: string): string {
  const name = path.split("/").pop()?.toLowerCase() ?? "";
  if (BY_NAME[name]) return BY_NAME[name];
  const dot = name.lastIndexOf(".");
  return dot >= 0 ? BY_EXTENSION[name.slice(dot + 1)] ?? "" : "";
}

/**
 * Highlights and splits the result into one balanced HTML string per line, so
 * a comment or string that spans lines keeps its color on every line. Only
 * highlight.js's escaped output is ever returned.
 */
export function highlightLines(code: string, language: string): string[] | null {
  if (!language || !hljs.getLanguage(language)) return null;
  const html = hljs.highlight(code, { language, ignoreIllegals: true }).value;
  const lines: string[] = [];
  const open: string[] = [];
  let line = "";
  const tag = /<span[^>]*>|<\/span>|\n/g;
  let last = 0;
  for (let m = tag.exec(html); m; m = tag.exec(html)) {
    line += html.slice(last, m.index);
    last = m.index + m[0].length;
    if (m[0] === "\n") {
      lines.push(line + "</span>".repeat(open.length));
      line = open.join("");
    } else if (m[0] === "</span>") {
      open.pop();
      line += m[0];
    } else {
      open.push(m[0]);
      line += m[0];
    }
  }
  lines.push(line + html.slice(last) + "</span>".repeat(open.length));
  return lines;
}
