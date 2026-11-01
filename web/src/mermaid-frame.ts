import mermaid from "mermaid";
import DOMPurify from "dompurify";

// This module runs only in a sandboxed, opaque-origin iframe with a restrictive CSP.
// Mermaid needs a DOM while rendering, so sanitizing after render alone is not a
// sufficient boundary for untrusted diagrams.
let resizeObserver: ResizeObserver | undefined;
window.addEventListener("message", async (event) => {
  if (event.source !== window.parent || typeof event.data?.code !== "string") return;
  const theme = event.data.theme === "dark" ? "dark" : "default";
  document.body.style.background = theme === "dark" ? "#22262e" : "#ffffff";
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: "strict",
    suppressErrorRendering: true,
    maxTextSize: 20000,
    maxEdges: 500,
    htmlLabels: false,
    theme,
  });
  try {
    const { svg } = await mermaid.render("meshra-diagram", event.data.code);
    const clean = DOMPurify.sanitize(svg, {
      USE_PROFILES: { svg: true, svgFilters: true },
      FORBID_TAGS: ["a", "image", "foreignObject"],
      FORBID_ATTR: ["href", "xlink:href", "target"],
    });
    document.body.innerHTML = clean;
    const diagram = document.querySelector("svg");
    if (!diagram) throw new Error("No diagram was produced");
    diagram.removeAttribute("height");
    diagram.style.width = "100%";
    diagram.style.height = "auto";
    const reportSize = () => {
      const height = Math.ceil(diagram.getBoundingClientRect().height) + 24;
      window.parent.postMessage({ type: "mermaid-rendered", height }, "*");
    };
    reportSize();
    resizeObserver?.disconnect();
    resizeObserver = new ResizeObserver(reportSize);
    resizeObserver.observe(diagram);
  } catch {
    document.body.replaceChildren();
    window.parent.postMessage({ type: "mermaid-error" }, "*");
  }
});
