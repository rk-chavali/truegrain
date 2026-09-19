import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

/*
  Component tests.

  Beside the code they test, as Metabase and Superset both do, and unit
  over end-to-end wherever the question can be answered without a
  browser. The rule that decides what gets a test here: if a bug in it
  would be a wrong answer rather than a wrong pixel, it is tested.
*/
export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    /*
      Stylesheets are stubbed, because a component test asserts
      behaviour rather than paint. tokens.css is the exception: the
      palette is asserted against WCAG in tokens.test.ts, and the
      blanket stub returns an empty string even for a ?raw import,
      which made that test pass over nothing.
    */
    css: { include: [/tokens\.css/] },
    coverage: {
      provider: "v8",
      reporter: ["text-summary"],
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/**/*.test.{ts,tsx}", "src/test/**"],
    },
  },
});
