import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// Restore the marker `//go:embed all:dist` points at.
//
// `emptyOutDir` wipes the output directory on every build, and the marker is
// the only committed file in it. Without this it comes back as a deletion in
// everybody's `git status` the first time they build the UI, and a fresh
// clone that has had one build in it no longer compiles.
function keepEmbedMarker(): Plugin {
  let outDir = "";
  return {
    name: "truegrain:keep-embed-marker",
    apply: "build",
    // Vite has already made this absolute, which is why it is read here
    // rather than recomputed from the relative path below.
    configResolved(config) {
      outDir = config.build.outDir;
    },
    closeBundle() {
      writeFileSync(resolve(outDir, ".gitkeep"), "");
    },
  };
}

// The console is built into the Go binary, so the output goes where the
// embed directive looks rather than into a local dist/. One artifact means
// the UI and the API it talks to cannot be different versions.
export default defineConfig({
  plugins: [react(), keepEmbedMarker()],
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
