/**
 * Vite build, local dev proxy, and Vitest configuration for the ReconcileX frontend.
 */
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],

  build: {
    // Preserve web/dist/.gitkeep during `vite build` so Go's `//go:embed all:dist`
    // always finds a valid directory even in a fresh repository checkout.
    emptyOutDir: false
  },

  server: {
    // Proxy API and probe routes to the Go server during local `npm run dev`
    // so frontend calls work identically to production same-origin hosting.
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
      "/readyz": "http://localhost:8080"
    }
  },

  test: {
    // Provide browser DOM globals (document, window, Event) for React component tests.
    environment: "jsdom"
  }
});
