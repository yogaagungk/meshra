import { defineConfig } from "vite";
import { fileURLToPath, URL } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
export default defineConfig({
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  plugins: [react(), tailwindcss()],
  base: "/console/",
  build: {
    outDir: "../internal/consoleui/dist",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        manualChunks: {
          markdown: ["react-markdown", "remark-gfm"],
          query: ["@tanstack/react-query"],
          primitives: ["@radix-ui/react-dialog"],
        },
      },
    },
  },
  server: {
    proxy: {
      "/console/api": {
        target: "http://127.0.0.1:7331",
        changeOrigin: true,
        configure(proxy) {
          proxy.on("proxyReq", (out, req) => {
            if (req.headers.origin)
              out.setHeader("Origin", "http://127.0.0.1:7331");
          });
        },
      },
    },
  },
});
