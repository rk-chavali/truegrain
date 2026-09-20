# truegrain: the design documents

These began as a specification written before the code and are now maintained
against it. Reviewed in full on 17 September 2026, when several of them had
drifted badly enough to mislead: one still said there was no UI, one documented
a repository layout that no longer existed, and one described six validation
suites as though all six ran.

If you find a claim here that the code contradicts, the code is right and this
folder has a bug. Say so.

For using truegrain rather than understanding it, start with `README.md` at the
repository root.

## Contents

| File | What it covers |
|---|---|
| `01-overview.md` | What this is, what it is not, and where it genuinely differs |
| `02-architecture.md` | Components, request lifecycle, deployment, layout |
| `03-semantic-model.md` | The Ossie subset, modelling rules, rollups, correctness traps |
| `04-interfaces.md` | MCP, REST, GraphQL, the Postgres wire protocol, CLI, clients |
| `05-governance.md` | Policy tags, identity, compile-time denial, audit |
| `06-validation.md` | What is proven, how, and what is not |
| `07-roadmap.md` | Shipped, next, and deliberately not built |
| `08-contributing.md` | Licensing, contribution flow, how to run it locally |
| `09-deploying.md` | TLS posture, probes, replicas, credentials, rotation, environments |
| `10-control-plane.md` | Accounts, sessions, invitations, and where warehouse credentials live |
| `11-feature-map.md` | What comparable layers do, what this one does, and the order worth building |
| `12-enterprise-readiness.md` | What others gate behind an enterprise tier, what self-hosting answers for free, and the console arrangement |

## The one-sentence version

An Ossie semantic model compiles to governed, dialect-correct SQL on BigQuery,
Snowflake, Postgres and DuckDB, served over MCP for agents and REST for
everything else, with column access resolved before a query is ever sent, and
questions it cannot answer correctly refused rather than answered wrongly.

## Non-negotiables

These constraints shape every other decision here. Do not relax one without
changing this file in the same commit.

1. **No `run_sql` escape hatch on any interface.** The only expressible request
   is a semantic one. A test asserts the MCP tool surface stays at four.
2. **Governance resolves at compile time.** An unauthorized plan never becomes
   SQL, and an error from a policy source is a denial rather than permission.
3. **Never a shared admin service account.** Queries execute as the calling
   workload identity where the warehouse allows it, and `health` states plainly
   when they do not.
4. **The model format is Ossie.** No proprietary YAML dialect.
5. **Correctness is proven, not claimed.** `06-validation.md` lists what is
   proven and what is not, and is the document to distrust first if it ever
   reads as though everything works.
6. **The audit log and telemetry carry no filter values, no SQL text and no
   rows.** Enforced by shape in `internal/observe`, and tested.
