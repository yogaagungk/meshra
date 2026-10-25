import React from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { App } from "./App";
import "@fontsource-variable/manrope";
import "./style.css";
import "./conversation-list.css";
import "./personal.css";
import "./ai-status.css";
import "./automation.css";
const client = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15000,
      retry: 1,
      refetchOnWindowFocus: true,
      gcTime: 120000,
    },
  },
});
class ErrorBoundary extends React.Component<
  { children: React.ReactNode },
  { error: boolean }
> {
  state = { error: false };
  static getDerivedStateFromError() {
    return { error: true };
  }
  render() {
    return this.state.error ? (
      <main className="fatal">
        <h1>The console couldn’t render this view.</h1>
        <p>Your messages remain in Meshra. Reload to reconnect.</p>
        <button onClick={() => location.reload()}>Reload console</button>
      </main>
    ) : (
      this.props.children
    );
  }
}
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary>
      <QueryClientProvider client={client}>
        <App />
      </QueryClientProvider>
    </ErrorBoundary>
  </React.StrictMode>,
);
