#!/usr/bin/env node
/*
  Starts a real truegrain console for the visual tests.

  The binary and the fixture model, not a dev server with mocked
  responses: a screenshot of a mock proves the mock renders. It builds
  the Go binary if one is not already there, creates a throwaway control
  database, claims the instance, and seeds enough content that the
  screens have something on them.

  Deterministic on purpose. Every value a screenshot can see is fixed,
  because a relative timestamp or a generated id in a baseline means a
  diff every time the suite runs.
*/

import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "..", "..");
const consoleDir = resolve(here, "..");
const port = process.argv[2] ?? "8123";

// Shared with e2e/console.spec.ts. Long enough for the control plane's
// minimum, and fixed so the suite can sign in as this account.
const MEMBER_PASSWORD = "a-long-enough-member-password";

const PG = process.env.TRUEGRAIN_E2E_PG ?? "postgres://postgres:postgres@127.0.0.1:5432";

// The control plane's own database: accounts, sessions, connections.
const CONTROL_DB = process.env.TRUEGRAIN_E2E_DB ?? "truegrain_e2e";

/*
  The warehouse the model reads, seeded from the same fixture the Go
  parity suite uses.

  Its own database rather than the truegrain_scratch a developer keeps
  for `go test`: this one is dropped and rebuilt on every run, and doing
  that to a database another suite is using would be rude on a laptop
  and wrong in CI. Seeding it here is also what makes the harness work
  on a machine that has never run the Go tests.
*/
const WAREHOUSE_DB = process.env.TRUEGRAIN_E2E_WAREHOUSE_DB ?? "truegrain_e2e_warehouse";
const WAREHOUSE = `${PG}/${WAREHOUSE_DB}?sslmode=disable`;

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: "utf8", ...opts });
  if (r.error) throw r.error;
  return r;
}

function give_up(reason) {
  console.error(`could not prepare the test database: ${reason}`);
  console.error("visual tests need a local PostgreSQL; see console/e2e/README.md");
  process.exit(1);
}

/*
  Finds psql.

  On Linux and in CI it is on PATH and this returns immediately. The
  Windows installer does not put it on PATH, so the versioned install
  directories are tried before giving up: otherwise the whole suite
  fails on a developer machine that has PostgreSQL running perfectly
  well.
*/
function findPsql() {
  const candidates = [process.env.PSQL, "psql"].filter(Boolean);

  if (process.platform === "win32") {
    const root = "C:/Program Files/PostgreSQL";
    if (existsSync(root)) {
      const versions = readdirSync(root)
        .filter((name) => /^\d+$/.test(name))
        .sort((a, b) => Number(b) - Number(a));
      candidates.push(...versions.map((v) => join(root, v, "bin", "psql.exe")));
    }
  }

  for (const candidate of candidates) {
    const r = spawnSync(candidate, ["--version"], { encoding: "utf8" });
    if (!r.error && r.status === 0) return candidate;
  }
  give_up("no psql on PATH, and none found in the usual install locations");
}

function psqlOrDie(psql, dsn, args) {
  const r = run(psql, [dsn, "-v", "ON_ERROR_STOP=1", "-q", ...args]);
  if (r.status !== 0) give_up(r.stderr.trim());
}

/*
  Fresh databases each run, so a screenshot never depends on what a
  previous run left behind.

  The warehouse gets testdata/fixtures/seed.sql, unmodified, which is
  the same file the DuckDB and Postgres parity suites load. That is
  deliberate: the fan-out these screenshots are about only exists
  because of how that fixture's line totals are arranged, and a
  separate copy would drift away from the thing being demonstrated.
*/
function resetDatabases() {
  const psql = findPsql();
  const admin = `${PG}/postgres?sslmode=disable`;

  for (const db of [CONTROL_DB, WAREHOUSE_DB]) {
    psqlOrDie(psql, admin, ["-c", `DROP DATABASE IF EXISTS ${db}`]);
    psqlOrDie(psql, admin, ["-c", `CREATE DATABASE ${db}`]);
  }

  psqlOrDie(psql, WAREHOUSE, ["-f", join(repo, "testdata", "fixtures", "seed.sql")]);
}

/*
  Builds the console into the directory the Go binary embeds.

  This has to happen before the Go build, not after: `serve console`
  serves //go:embed dist, so skipping it means the suite screenshots
  whatever bundle was built last rather than the working tree. That is
  not hypothetical. It is how a stylesheet fix sat in the source while
  the tests kept failing against the old one.
*/
function frontend() {
  // vite through node rather than `npm run build`, because node
  // refuses to spawn npm.cmd without a shell on Windows and a shell
  // here is a quoting problem waiting to happen.
  const vite = join(consoleDir, "node_modules", "vite", "bin", "vite.js");
  const r = run(process.execPath, [vite, "build"], { cwd: consoleDir, stdio: "inherit" });
  if (r.status !== 0) {
    console.error("the console did not build");
    process.exit(1);
  }
}

function binary() {
  const out = join(
    repo,
    "build",
    process.platform === "win32" ? "truegrain-e2e.exe" : "truegrain-e2e",
  );
  mkdirSync(dirname(out), { recursive: true });
  // -buildvcs=false because this binary is a test fixture, not a release.
  // The screenshot job runs in the Playwright container, where git sees the
  // checkout as owned by another user and exits 128, and Go reports that as
  // a build failure rather than skipping the stamp it cannot read.
  const r = run("go", ["build", "-buildvcs=false", "-o", out, "./cmd/truegrain"], {
    cwd: repo,
  });
  if (r.status !== 0) {
    console.error(r.stderr);
    process.exit(1);
  }
  return out;
}

async function post(path, body, cookie) {
  const res = await fetch(`http://127.0.0.1:${port}${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Truegrain-Console": "1",
      ...(cookie ? { Cookie: cookie } : {}),
    },
    body: JSON.stringify(body),
  });
  return res;
}

async function waitForServer() {
  for (let i = 0; i < 120; i++) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/api/bootstrap`);
      if (res.ok) return;
    } catch {
      // not up yet
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error("the console did not start");
}

/*
  Fixed content, so the baselines are stable.

  The three queries at the end matter: one answers, one groups, and one
  is the canonical fan-out, which puts a real refusal in the audit log
  for the Activity screenshot to find.
*/
async function seed() {
  const claim = await post("/api/auth/claim", {
    org: "Acme Analytics",
    email: "e2e@truegrain.test",
    name: "Screenshot",
    password: "a-long-enough-e2e-password",
  });
  const cookie =
    claim.headers.getSetCookie?.().join("; ") ?? claim.headers.get("set-cookie") ?? "";

  // Left outstanding, so the People screen has an invitation on it.
  await post(
    "/api/people/invite",
    { email: "analyst@acme.test", role: "member", groups: ["analysts"] },
    cookie,
  );

  /*
    A second invitation, accepted, so there is a real member account.

    It exists for one test: the audit log is readable by owners and
    admins, and a member must be told they are being refused access
    rather than told the capability is missing. Without an account at
    that role there is no way to exercise the difference against the
    real product.

    The group is deliberately "role:admin". That is the forged group the
    Go tests cover in isolation, put here so the end to end suite proves
    the stripping survives a real invitation and a real login.
  */
  const invited = await post(
    "/api/people/invite",
    { email: "member@acme.test", role: "member", groups: ["role:admin"] },
    cookie,
  );
  const { accept_path: acceptPath } = await invited.json();
  const accepted = await post(`/api/invites/${acceptPath.split("/").pop()}/accept`, {
    name: "Member",
    password: MEMBER_PASSWORD,
  });
  if (!accepted.ok) {
    console.error(`could not seed the member account: ${accepted.status}`);
    process.exit(1);
  }
  await post(
    "/api/connections",
    {
      name: "warehouse",
      dialect: "postgres",
      secret: "postgres://user:password@warehouse.internal:5432/analytics",
      detail: { host: "warehouse.internal", database: "analytics" },
    },
    cookie,
  );

  for (const body of [
    { metrics: ["retail.order_revenue"] },
    { metrics: ["retail.order_revenue"], dimensions: ["retail.customers.region"] },
    { metrics: ["retail.order_revenue"], dimensions: ["retail.order_lines.item_id"] },
  ]) {
    await post("/v1/query", body, cookie);
  }
}

resetDatabases();
frontend();
const bin = binary();

const child = spawn(
  bin,
  [
    "serve",
    "console",
    "-models",
    join(repo, "testdata", "models"),
    "-addr",
    `127.0.0.1:${port}`,
    "-dialect",
    "postgres",
    "-dsn-env",
    "TRUEGRAIN_E2E_WAREHOUSE_DSN",
    // Assertions, so the Checks screen has a suite to run rather than
    // only ever reporting that none is configured.
    "-tests",
    join(here, "fixtures", "tests.yaml"),
    // An hour, so exactly the immediate check fires and the history the
    // screen renders is one run rather than a racing number of them.
    "-doctor-every",
    "1h",
  ],
  {
    stdio: "inherit",
    env: {
      ...process.env,
      TRUEGRAIN_CONTROL_KEY: "an-end-to-end-test-key-that-is-long-enough",
      TRUEGRAIN_CONTROL_DSN: `${PG}/${CONTROL_DB}?sslmode=disable`,
      TRUEGRAIN_E2E_WAREHOUSE_DSN: WAREHOUSE,
    },
  },
);

child.on("exit", (code) => process.exit(code ?? 0));
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => child.kill(signal));
}

await waitForServer();
await seed();
console.log(`console ready on ${port}, seeded`);
