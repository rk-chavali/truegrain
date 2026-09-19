# Contributing

## Getting it running

No cloud account, no credentials, no C compiler.

```bash
make build
make test          # everything that needs no database
make duckdb        # single executable into bin/, no installer
make test-all      # adds the parity suite against real DuckDB
make demo          # seeds a fixture warehouse and runs a query end to end
make teams         # the two-team workspace: namespaces, imports, governance
```

## Sign your commits off

We use a [DCO](https://developercertificate.org/) rather than a CLA. Add a
sign-off line with `git commit -s`. That is the whole process.

## What a change needs

A pull request is ready when all of these are true. They are the same rules the
existing code follows, and a reviewer will check them.

- **It runs.** You executed the tests, not just read them.
- **New behaviour has a test that would fail if the behaviour regressed.** Not a
  test that exercises the code: one that fails when it is wrong.
- **Golden SQL is regenerated and reviewed.** `make golden`, then read the diff.
  A change there means compiled output changed, which is the highest-stakes kind
  of change this project has.
- **Errors are handled where they can be acted on**, and refusals carry a code,
  a reason and a hint that names what to do instead.
- **Input from outside the process is validated at the edge**, before it reaches
  planning.
- **No dead code, no commented-out blocks, no debug prints.**

## Things that are not style preferences

These have caused real bugs here. A change that breaks one will be sent back.

**Governance runs before emission, always.** Every query goes through
`engine.Compile`, which checks *every part* of a plan before any of it becomes
SQL. `TestGovernanceAppliesToEveryPart` exists because checking only the first
part would let every later metric escape the gate.

**Policy resolves against where a field was defined.** An imported dataset keeps
the identity it was defined with. Resolving against the local alias would make
`as:` an access control bypass.

**Every path that makes an access decision audits it.** `truegrain compile` once
discarded denials silently. The audit sink is now owned by the engine builder
precisely so that no command can forget.

**A refusal is never an empty result.** Returning no rows for a question the
engine cannot answer trains a caller to report a wrong number confidently.

**No interface accepts SQL.** Four MCP tools, and no endpoint takes a SQL
string. `TestNoRawSQLTool` and `TestNoEndpointAcceptsSQL` guard it. This is the
entire argument for routing an agent through a semantic layer, and it survives
exactly as long as the escape hatch stays absent.

**The API contract cannot drift.** `api/openapi.yaml` is checked against the
routes the server registers, in both directions. The Python SDK is checked
against the spec.

## Where things live

```
cmd/truegrain              validate | compile | query | health | serve
internal/osi              Ossie IR, YAML parser, expression parser
internal/resolve          join graph, fan-out classification
internal/plan             dialect-free planner. No SQL strings live here.
internal/dialect          emitters, per-warehouse syntax, multi-fact combiner
internal/govern           the gate, policy resolvers, audit
internal/engine           the composition that makes the gate unbypassable
internal/workspace        namespaces, imports, origin tracking
internal/serve/{mcp,rest} interfaces
api/openapi.yaml          the contract SDKs are built against
api/openapi.yaml          the HTTP contract
docs/                     design specification
internal/osi/COMPLIANCE.md  where this engine diverges from the Ossie spec
```

`internal/plan` holds no SQL strings, so correctness is unit-testable without a
database. Keep it that way.

## Opening a change

Open an issue first for anything beyond a typo. For a change to the plan object,
to Ossie construct support, to the governance contract, or to anything that
alters a compiled SQL result, write an RFC in `rfcs/` before the code. That is
heavier than a project this size normally needs, and it is worth it for one
reason: this engine's output is numbers people make decisions on, and an
undocumented change to a compiled result is a silent correctness incident.

Labels that get used: `good first issue`, `dialect`, `planner`, `governance`,
`osi-compliance`, `correctness`, `needs-repro`.

## Reporting a wrong number

The most valuable bug report this project can get. Include the model, the
request, the compiled SQL from the response, the `model_version`, and what you
expected. Every query response carries the first three, which is why they are
there.
