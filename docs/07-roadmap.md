# 07 Roadmap

Rewritten on 18 September 2026 against what the code actually does. The
version before this one described a plan while it was still a plan, and
carried on describing it for two releases afterwards. It listed six things
under "Next" that had shipped and two under "Deliberately not built" that
had also shipped.

## Shipped

**v0.1.0.** Ossie parser and workspace composition, resolver, planner with
fan-out and chasm detection, four dialects, the compile-time governance gate,
BigQuery policy tags, MCP, REST, CLI, and the parity suite on DuckDB.

**v0.2.0.** Validated against a real BigQuery warehouse for the first time,
which found three bugs no mock would have. Structured logging and OpenTelemetry.
JSON and markdown output on `validate` and `diff`, and a GitHub Action.
Refusals recorded in the audit log and readable over the API.

**v0.3.0.** `truegrain doctor`, which checks the model against the live
warehouse. Model reload without a restart, keeping the previous model when a
new one fails to load. Scoped API tokens, so a CI credential can read the
model without being able to run a query. Row-level policies. `truegrain
init`, which reads a warehouse and writes the model. Asynchronous jobs, OIDC
and Google identity token authentication, Postgres and Snowflake dialects,
release engineering with build attestations.

Published alongside: clients on PyPI, npm and the Go module proxy, a console
in `truegrain-console`, and multi-arch container images on ghcr.io.

**Unreleased, on `main`.** The largest block since v0.1.0, and most of it is
things the previous roadmap listed as not built.

- **The PostgreSQL wire protocol.** `truegrain serve postgres`. A BI tool can
  connect. See [04-interfaces.md](04-interfaces.md) for what the synthesised
  catalogue answers and what it declines.
- **Importers** for dbt, LookML and Cube. The highest-value item on the old
  list for anyone with an existing semantic layer, and it existed only as a
  note saying so.
- **Rollups.** Pre-aggregated tables, routed to only when a declared
  freshness check passes, and only for metrics that can be recomputed from
  partial aggregates. The old roadmap called this out as needing to be
  correct about non-additive measures, which was right, and that is where
  most of the work went.
- **A result cache**, after the governance gate rather than before it.
- **Durable jobs**, shared across replicas through Postgres, so a caller can
  submit to one replica and poll another.
- **Model unit testing.** `truegrain test` asserts what a metric answers,
  including that a refusal happens and that a row policy bites.
- **GraphQL**, as one query field, for a front end that already speaks it.
- **Mutual TLS**, an audit webhook, and bounded warehouse concurrency.
- **Secret rotation** through a credentials file re-read while the engine
  runs, and **environments**, so dev and prod come from one set of model
  files.
- **Six more dialects**: Redshift, Databricks, ClickHouse, Athena, Trino, and
  a Snowflake executor over the SQL REST API.

## Not shipped

**No executor has been verified for Snowflake, Databricks, ClickHouse,
Athena or Trino.** All six dialects compile and are golden tested. Snowflake
has an executor; the other five have none. Every one of them reports this in
its capabilities rather than leaving it to be discovered, because that is
exactly what was true of Postgres until somebody ran it and found a whole
dialect had never executed a statement.

**The real-GCP governance suite is partly done.** Policy tags are read and
enforced against a real taxonomy. Fine-grained reader enforcement is
unverified because it needs an organizational project.
[06-validation.md](06-validation.md) says which is which. There is still no
Terraform fixture.

**psql's `\d <table>` is refused.** It sends a join across half a dozen
catalogue tables with regex operators, and the synthesised catalogue
declines rather than approximating. `\dt` works, and so does reading
`information_schema.columns` directly.

**No Java client.** Nobody has asked.

## Next, in order

1. **Run Snowflake against a real account.** The executor is written and
   every part of it that is this engine's fault is tested. The part that is
   not, that Snowflake accepts what is sent, needs an account, a warehouse
   and a registered key.
2. **A Terraform fixture for the GCP governance suite**, so the one
   unverified governance claim stops being unverified.
3. **Executors for Databricks and Trino.** Both are ordinary once somebody
   has a cluster to point at, and between them they cover most of what the
   four unexecuted dialects are for.
4. **Console screens** for refusals, activity and spend. All three have data
   behind them.
5. **A `truegrain init` reader for Snowflake and DuckDB.** It reads BigQuery
   and PostgreSQL today and the shape is already warehouse-agnostic.

## Deliberately not built

**No visual model editor.** Strata owns authoring: it introspects a
warehouse, holds the model, and emits Ossie. A drag-and-drop editor here
would fight the premise that the model lives in Git and changes through
review.

**The wire protocol is not a SQL proxy.** The catalogue answers what a client
needs to connect and list fields, correctly, and declines the rest. Being
pg_catalog for everything a tool might ask means being PostgreSQL, and every
answer would be a fiction that some tool eventually depends on.

**A rollup is never trusted without a freshness check.** There is no flag to
turn that off. A pre-aggregated table nobody checks is one nobody notices has
stopped building, and the difference between that and having no rollups is
that one of them returns wrong numbers.

## What the earlier plan got wrong

Worth keeping, because it was right and it took two releases to act on.

It said: "The generic Ossie-to-SQL compiler is table stakes and will be
commoditized. Governance and importers are where the durable value sits. If
time runs short, cut features from the compiler before cutting either of
those."

Then we cut importers and built more compiler. Importers exist now, for dbt,
LookML and Cube. Governance was written but unproven until v0.2.0. The
compiler is excellent and is still the part least likely to matter.
