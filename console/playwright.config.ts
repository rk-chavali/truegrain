import { defineConfig, devices } from "@playwright/test";

/*
  Visual regression and accessibility.

  Committed baseline screenshots that must not change silently, which is
  the same gate the engine already has for its compiled SQL: `go test
  ./internal/dialect/ -update` then a diff that must be empty. A UI
  change should be as readable in a pull request as a SQL change.

  Playwright rather than Chromatic or Percy. It is free, self-hosted and
  needs no account, which matches this project's posture everywhere
  else; a self-hosted tool whose CI depends on a SaaS screenshot service
  is a contradiction. The cost is that baselines are pixels in git
  rather than a review UI, so the shots are deliberately few and
  component-scoped.

  The app under test is the real binary serving the real console, not a
  dev server with mocks. A screenshot of a mocked page proves the mock
  renders.
*/

const PORT = 8123;

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : [["list"]],

  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    /*
      A fixed viewport and a disabled caret, because a blinking cursor
      and a scrollbar that appears at one width are the two things that
      make a pixel diff flake.
    */
    viewport: { width: 1280, height: 860 },
    deviceScaleFactor: 1,
  },

  expect: {
    toHaveScreenshot: {
      /*
        A small tolerance. Font rasterisation differs by a hair between
        machines even with the same font file, and a zero-tolerance
        baseline fails on somebody else's laptop for no reason anybody
        can act on.
      */
      maxDiffPixelRatio: 0.01,
      animations: "disabled",
      caret: "hide",
      scale: "css",
    },
  },

  projects: [
    /*
      Sign in once, then run everything against the saved session.

      The control plane rate limits logins, correctly, and a suite where
      every test signed in for itself started tripping it once there were
      a dozen tests. Reusing the session is also closer to how the
      product is used.
    */
    {
      name: "setup",
      testMatch: /auth\.setup\.ts/,
      use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 860 } },
    },
    {
      name: "chromium",
      dependencies: ["setup"],
      testMatch: /console\.spec\.ts/,
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1280, height: 860 },
        storageState: "e2e/.auth/owner.json",
      },
    },
  ],

  /*
    Started by the harness rather than by hand, so `npm run test:visual`
    works from a clean checkout. See e2e/server.ts for why this needs a
    database and what it does about it.
  */
  webServer: {
    command: `node ./e2e/server.mjs ${PORT}`,
    port: PORT,
    reuseExistingServer: !process.env.CI,
    /*
      Generous, because this is not a server starting: it is a
      PostgreSQL fixture load, a Vite build and a Go build, and only
      then a listener. In a cold container the Go build alone has run
      past two minutes, which is what the old 120s limit was set for
      back when the harness did not build the frontend at all.
    */
    timeout: 420_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
