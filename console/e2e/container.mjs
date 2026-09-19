#!/usr/bin/env node
/*
  Runs the visual suite inside the official Playwright container.

  Screenshot baselines are platform specific, and not by accident:
  Chromium rasterises text differently on Windows, macOS and Linux, so a
  baseline taken on a laptop can never match a Linux CI runner.
  Playwright encodes that in the filename, which is why the committed
  snapshots are suffixed -chromium-linux.

  So the baselines are generated in the same image CI runs in. That is
  the whole point of this script: one pinned image, used both to write
  the baselines and to check them. Anything else and "the screenshots
  differ" means "two machines" rather than "the UI changed".

  Postgres runs in a container too, on a private network, rather than
  the one on the developer's machine. Not for isolation: a host
  Postgres rejects the container outright, because pg_hba admits
  127.0.0.1 and the container arrives from somewhere else. Running it
  here also makes this the same shape as CI, where Postgres is a
  service container.

  Usage:
    node e2e/container.mjs                     check against the baselines
    node e2e/container.mjs --update-snapshots  write them

  Everything after the script name is passed through to `playwright test`.
*/

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const consoleDir = resolve(here, "..");
const repo = resolve(consoleDir, "..");

const NETWORK = "truegrain-e2e-net";
const PG_CONTAINER = "truegrain-e2e-postgres";
const PG_IMAGE = "postgres:17";

function docker(args, opts = {}) {
  const r = spawnSync("docker", args, { encoding: "utf8", ...opts });
  if (r.error) {
    console.error(`could not run docker: ${r.error.message}`);
    process.exit(1);
  }
  return r;
}

function read(path) {
  return readFileSync(path, "utf8");
}

/** Blocks the thread. Polling a health check is the one place that is what is wanted. */
function sleep(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

const playwrightVersion = JSON.parse(
  read(resolve(consoleDir, "node_modules", "@playwright", "test", "package.json")),
).version;
const image = `mcr.microsoft.com/playwright:v${playwrightVersion}-noble`;

// The container needs the toolchain the module declares, and Ubuntu's
// apt is several minor versions behind it.
const goVersion = /^go (\d+\.\d+\.\d+)/m.exec(read(resolve(repo, "go.mod")))?.[1];
if (!goVersion) {
  console.error("could not read the Go version from go.mod");
  process.exit(1);
}

function teardown() {
  docker(["rm", "-f", PG_CONTAINER], { stdio: "ignore" });
}

function startPostgres() {
  // Left over from an interrupted run, if the process was killed before
  // its teardown.
  teardown();
  docker(["network", "create", NETWORK], { stdio: "ignore" });

  const r = docker([
    "run",
    "-d",
    "--rm",
    "--name",
    PG_CONTAINER,
    "--network",
    NETWORK,
    "-e",
    "POSTGRES_PASSWORD=postgres",
    "--health-cmd",
    "pg_isready -U postgres",
    "--health-interval",
    "2s",
    "--health-retries",
    "30",
    PG_IMAGE,
  ]);
  if (r.status !== 0) {
    console.error(`could not start ${PG_IMAGE}: ${r.stderr.trim()}`);
    process.exit(1);
  }

  process.stdout.write("waiting for postgres");
  for (let i = 0; i < 60; i++) {
    const health = docker([
      "inspect",
      "-f",
      "{{.State.Health.Status}}",
      PG_CONTAINER,
    ]).stdout.trim();
    if (health === "healthy") {
      process.stdout.write(" ready\n");
      return;
    }
    process.stdout.write(".");
    sleep(1000);
  }
  console.error("\npostgres never became healthy");
  teardown();
  process.exit(1);
}

/*
  Three named volumes, all caches except one.

  node_modules is not a cache: it must be shadowed. The host's copy
  holds Windows or macOS binaries for esbuild and rollup, and
  bind-mounting those into Linux fails in a way that reads like a
  corrupt install rather than a wrong platform.
*/
const script = `
set -euo pipefail

if [ ! -x /usr/local/go/bin/go ]; then
  echo "installing Go ${goVersion}"
  curl -fsSL "https://go.dev/dl/go${goVersion}.linux-amd64.tar.gz" | tar -xz -C /usr/local/go --strip-components=1 go
fi
export PATH="/usr/local/go/bin:$PATH"

if ! command -v psql > /dev/null; then
  echo "installing the PostgreSQL client"
  apt-get update -qq && apt-get install -y -qq postgresql-client > /dev/null
fi

cd /work/console
npm ci --no-audit --no-fund
npx playwright test ${process.argv.slice(2).join(" ")}
`;

startPostgres();

const run = docker(
  [
    "run",
    "--rm",
    "--init",
    "--network",
    NETWORK,
    "-v",
    `${repo}:/work`,
    "-v",
    "truegrain-e2e-node-modules:/work/console/node_modules",
    "-v",
    "truegrain-e2e-go:/usr/local/go",
    "-v",
    "truegrain-e2e-gocache:/root/.cache/go-build",
    "-e",
    `TRUEGRAIN_E2E_PG=postgres://postgres:postgres@${PG_CONTAINER}:5432`,
    // Playwright's own CI switches: no reused server, no test.only.
    "-e",
    "CI=1",
    image,
    "bash",
    "-lc",
    script,
  ],
  { stdio: "inherit" },
);

teardown();
process.exit(run.status ?? 1);
