import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The console is built into the Go binary, so the output goes where the
// embed directive looks rather than into a local dist/. One artifact means
// the UI and the API it talks to cannot be different versions.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: process.env.CONSOLE_OUT ?? "../internal/console/dist",
    emptyOutDir: true,
    // No source maps in the shipped binary. They would roughly double the
    // embedded size and hand a reader the whole front end source.
    sourcemap: false,
  },
  server: {
    port: 5180,
    // In development Vite serves the pages and the Go process serves the
    // data, so both halves are live-reloadable. Same-origin through the
    // proxy, which means the session cookie works exactly as it does in
    // production and the CSRF header is not a special case.
    proxy: {
      "/api": "http://127.0.0.1:8080",
      "/v1": "http://127.0.0.1:8080",
    },
  },
});
