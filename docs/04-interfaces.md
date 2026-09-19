# 04 Interfaces

One planner behind every surface. No surface may expose a capability the others
do not, and none of them exposes raw SQL.

## What exists

| Surface | State |
|---|---|
| MCP server, four tools | built |
| REST and JSON, twelve endpoints | built |
| CLI | built |
| Python, TypeScript and Go clients | built and published |
| Console | built, in `truegrain-console` |
| Postgres wire protocol | built, `truegrain serve postgres` |
| GraphQL | built, `POST /graphql`, one query field |

The composition is the security boundary. Each surface is handed an `Engine`
and nothing else: none of them holds a `Dialect` or an `Executor`, so none can
produce SQL without the governance gate having run. Governance that relies on
every caller remembering to call a checker is governance that gets forgotten.

## 1. MCP server

Primary interface. Transport: streamable HTTP for the server, stdio for local
development.

### Tool surface

```
list_metrics(search?: string)
  -> [{ name, label, description, type, owner, dimensions: [string] }]

describe_metric(name: string)
  -> { name, description, definition, additivity, grain,
       dimensions: [{name, type, granularities?}], synonyms, owner }

list_dimensions(metric?: string)
  -> [{ name, type, granularities?, description }]

query(metrics: [string], dimensions?: [string], filters?: [Filter],
      grain?: string, limit?: number, order_by?: [Order])
  -> { rows, columns, compiled_sql, model_version, row_count }
```

`Filter` is structured, never a SQL fragment:

```json
{ "dimension": "order_status", "op": "in", "values": ["shipped","delivered"] }
```

Supported ops: `eq`, `ne`, `in`, `not_in`, `gt`, `gte`, `lt`, `lte`, `between`,
`is_null`, `is_not_null`.

### Design rules

- **There is no `run_sql` tool and there will never be one.** An agent cannot
  express an ungoverned query because the request vocabulary does not contain
  one. This is the entire argument for routing an agent through a semantic layer
  instead of at the warehouse. It survives exactly as long as the escape hatch
  stays absent.
- Tool descriptions are written for a model, not a human. Include when to use
  the tool and when not to.
- `describe_metric` returning rich descriptions is what makes the agent pick the
  right metric. Treat description quality as a product feature.
- Errors are structured and actionable: which metric, which dimension, what was
  wrong, what to try instead. A model reads the error and retries, so a vague
  error costs a round trip.
- Governance denials return a refusal with a reason, never an empty result set.
  Silent nulls train an agent to report wrong answers confidently.

## 2. Postgres wire protocol

`truegrain serve postgres` speaks the PostgreSQL wire protocol, so any SQL
client connects with no integration work. psql, pgx, JDBC and the BI tools
built on them.

```bash
truegrain serve postgres -addr 127.0.0.1:5439 -models ./models -insecure
psql "postgres://tester@127.0.0.1:5439/truegrain?sslmode=disable"
```

Port 5439 rather than 5432, so it cannot be mistaken for a PostgreSQL
server on the same host. The bearer token goes in the password field,
which is the only place in the protocol a client will put a secret and the
one box every BI tool has.

### Presentation model

- One schema, `public`.
- Each namespace appears as one table whose columns are its metrics and
  dimensions. That is the shape the grammar accepts, so a tool that lists
  and then selects writes a statement this can answer.
- A metric is typed `numeric` and a dimension `text`. Reporting a measure
  as text is what makes a BI tool file it under categories, so the type
  comes from the model rather than from the values that came back.

### Translation

Incoming SQL is parsed into a `plan.Request` of names that must resolve
against the model. It is never passed through, and there is no
concatenation path from anything a caller sends to a statement the
warehouse runs.

- The `SELECT` list maps to metrics and dimensions.
- `WHERE` maps to structured filters, whose values become bound
  parameters.
- `GROUP BY` is implied by the dimensions selected and may be omitted.
- `date_trunc(...)` on a time dimension maps to grain.
- Anything outside the grammar is **refused**, with the same code and hint
  a REST caller gets. A fan-out is refused here exactly as it is
  everywhere else; that is the point of one request vocabulary.

### The catalogue

A client asks several questions before its first real one, and the answers
are synthesised from the model. `pg_namespace`, `pg_class`,
`pg_attribute`, `pg_type`, `pg_tables` and the `information_schema` views
are relations holding rows derived from what the caller may see.
`pg_views`, `pg_indexes` and `information_schema.views` exist and are
empty, which is the truth: a namespace has no view definition and no
index.

A restricted SELECT reads them: projection, aliases, a conjunction of
simple predicates, `ORDER BY`, `LIMIT`. Anything it does not fully
understand it declines, and declining is deliberate. The alternative,
answering approximately, means silently dropping a `WHERE` clause and
handing a BI tool a field picker full of the wrong fields, which nothing
in the protocol reports.

What it declines is real and common: joins across catalogue tables, `CASE`
expressions, casts, `OR`, `ANY()` over an array, subqueries, `OFFSET`,
aggregation. psql's `\dt` is recognised by fingerprint. psql's `\d
<table>` is not, because it sends a join across half a dozen catalogue
tables with regex operators, and parsing that means being a database.

A statement the catalogue cannot answer is logged at warn level with the
name the client gave itself, so an operator finds out that Tableau sent a
`pg_table_is_visible` join rather than hearing that the connection does
not work.

## 3. REST / JSON

Twelve routes. `api/openapi.yaml` is the contract, and
`TestOpenAPIMatchesRoutes` asserts in both directions that the document and the
mux agree, so this table cannot drift from the server without a test failing.

### Metadata

```
GET  /v1/health            what this deployment enforces, and what it does not
GET  /v1/model/version     the workspace digest currently served
GET  /v1/namespaces        who owns what, and which namespaces loaded
GET  /v1/metrics           every metric this identity may read
GET  /v1/metrics/{name}    one metric, and the dimensions legal for it
GET  /v1/dimensions        what is available for grouping and filtering
```

### Asking questions

```
POST /v1/query             answer now, holding the connection open
POST /v1/compile           the SQL it would run, without running it
```

The request body for both mirrors the MCP `query` tool exactly. One schema,
two transports.

`compile` is not a way to read SQL you may not run: the governance gate runs on
the same path, so a denied request returns a refusal and no SQL. It is audited
as `compiled` rather than `allowed`, which keeps looking distinguishable from
executing.

### Questions that outlive a request

```
POST   /v1/jobs            start a query, return an identifier
GET    /v1/jobs/{id}       poll it, and read one page of rows
DELETE /v1/jobs/{id}       cancel it
```

A warehouse query can run longer than the default HTTP timeout of every agent
framework in use. Synchronously the client gives up, retries, and the warehouse
runs and bills the query twice for an answer nobody reads.

Three properties are load bearing:

- **Compilation is synchronous**, so the gate runs before the job exists. A
  request the caller may not make is refused on submit rather than discovered
  by polling. A 202 is a promise the query was authorized.
- **A job is readable only by the identity that submitted it.** Another caller
  gets 404 rather than 403, so job identifiers cannot be probed for existence.
- **Abandoning a wait cancels the job**, rather than leaving the warehouse
  spending on an answer nobody is waiting for.

`row_count` is the size of the whole result rather than of the page, so a
caller knows how much remains before walking it. Polling a failed job is a 200
carrying `state: "failed"`: the poll succeeded, and the failure is described
with the same `code`, `hint` and `retry` a synchronous refusal carries.

MCP deliberately does not get this. An MCP client is a local process over stdio
with no request timeout to outlive.

### Who is calling

Three authenticators, narrowing as they get weaker.

| Flag | Identity | Use |
|---|---|---|
| `-oidc-issuer` + `-oidc-audience` | verified per caller | Okta, Entra, Auth0, Keycloak, Google |
| `-google-audience` | verified per caller | Cloud Run, GKE, Compute Engine |
| `-token-env` + `-identity` | one shared subject | a single tenant, nothing more |

Verification is delegated to `go-oidc` and Google's own verifier rather than
hand written. Four things fail closed rather than defaulting permissive: a
missing audience, a symmetric or `none` algorithm, a plaintext issuer, and an
`email` claim the provider has not marked verified. Serving on anything but
loopback with no authentication is refused outright.

A static token requires `-identity`, because a token is an identity claim: it
says "I am X". Without X every audited decision records an empty subject, which
cannot answer the one question an audit log exists to answer.

See `05-governance.md` for what happens to the identity once it exists.

### Reading what the engine decided

```
GET  /v1/audit             recent decisions, newest first
```

Every decision: allowed, refused, denied, compiled or failed. `refused` and
`denied` are separate on purpose. Denied is access, meaning this caller may not
read something. Refused is correctness, meaning nobody can be told this
accurately and the hint names a question that can be answered. Reporting them
together would count every fan-out as an access incident.

This is off until an operator names who may read it with `-audit-readers`,
because the record contains every caller's activity and so discloses what other
teams query and which fields are protected. With nobody named it answers 404,
since a capability nobody configured does not exist. An authenticated caller
who is not a named reader gets 403, and an anonymous caller is never a reader.

It is a bounded in-memory window rather than the whole record. An engine
started with `-audit` keeps everything in that file; this is what an operator
can see without shelling into the container.

## 4. Client SDKs

Each client lives in its own repository and releases on its own cadence, so a
Java contributor does not clone Go to fix a Python bug.

| Language | Install | Repository |
|---|---|---|
| Python | `pip install truegrain` | `rk-chavali/truegrain-python` |
| TypeScript | `npm install truegrain` | `rk-chavali/truegrain-typescript` |
| Go | `go get github.com/rk-chavali/truegrain-go` | `rk-chavali/truegrain-go` |

They are thin clients over REST with no compilation logic, and they are
hand written rather than generated. A generator produces a transport and a flat
error type; the parts worth having are the ones it cannot produce.

```python
from truegrain import Client, filters

c = Client.from_env()          # SEMANTIC_URL, SEMANTIC_TOKEN
result = c.run(                # submits, polls, pages, returns one result
    metrics=["sales.order_revenue"],
    dimensions=["sales.customers.region"],
    filters=[filters.eq("sales.orders.status", "shipped")],
)
result.to_dataframe()          # compiled_sql and model_version ride in df.attrs
```

`run()` exists so no caller writes a polling loop. A refusal carries
`should_modify()`, `should_wait()` and `is_final()`, which is the difference
between a self-correcting agent loop and an infinite one.

**How they stay in step.** Each client vendors `api/openapi.yaml`, pinned to an
engine commit, and tests itself against it in both directions: an operation the
engine gains with no client method fails that client's build, and a method
claiming an operation the spec does not define fails it too. A scheduled job
compares the pin against this repository daily and opens a pull request when
the contract moves. This repository keeps the other half, spec against routes.

CLI mirrors the same calls for CI and shell use:

```
truegrain init     -dialect bigquery -project acme -dataset main
truegrain validate -models ./models
truegrain compile  -models ./models -metric net_revenue -dim region -grain month -dialect bigquery
truegrain query    -models ./models -metric net_revenue -dim region -grain month -format csv
truegrain diff     -models ./models -against ./baseline
truegrain doctor   -models ./models -dialect bigquery -project acme
```

`truegrain compile` printing SQL without executing is the debugging tool people
will use most. Its output is copy-paste runnable.

`init` is the only command that writes files, and the only one a person runs
once rather than repeatedly. It reads the warehouse's metadata and produces a
model: datasets, columns, types, primary keys and foreign keys, and an empty
metrics file. Metrics are never generated, because which total the business
calls revenue is not derivable from a schema.

### What each command may reach

Worth stating here because it decides how a pipeline is credentialed, and
getting it wrong means a job that only checks a model holds a credential that
can read every row.

| Command | Reaches the warehouse for |
|---|---|
| `validate`, `diff`, `compile` | nothing; they read YAML and emit SQL |
| `init`, `doctor` | metadata only, through the API; no query job, no rows |
| `query`, `serve` | rows, via jobs it bills |

The roles that correspond, and the Workload Identity Federation setup that
avoids a key file in CI, are in `README.md` under "Which commands need
warehouse credentials".

## Upstream integrations, not downstream

dbt and Dataform build the tables this reads. They sit upstream. Querying the
semantic layer from inside a dbt model is circular.

The two real integrations run the other direction:

1. **Importers.** Read dbt, Dataform, LookML or Cube metadata and generate OSI
   definitions. This is the cold-start fix and may be more valuable than the
   engine itself.
2. **Rollup export.** Materialize a truegrain query back into the warehouse as a
   table that dbt or Dataform can then reference. Optional, v2.
