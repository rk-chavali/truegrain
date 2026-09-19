# truegrain

An Apache Ossie semantic model compiles to governed, dialect-correct SQL for
BigQuery, Postgres and Snowflake, served over MCP for agents and JSON over HTTP
for everything else, with column access resolved before a query is ever sent.

BigQuery and Postgres execute, and both are checked against the same fixture as
DuckDB. Snowflake compiles and has no executor yet.

DuckDB is in here too, as the local one: it makes the quickstart below need no
cloud account, and it is what the correctness suite executes against, so the
refusals are tested rather than asserted. Nobody should serve a company from it.

Status: working engine, v1 scope in progress. See [what is not built](#not-built-yet).

## What it does

```
$ truegrain compile -models ./models \
    -metric order_revenue,order_count \
    -dim customers.region,orders.order_date -grain month -limit 20

SELECT
  "customers"."region" AS "region",
  date_trunc('month', "orders"."order_date") AS "order_date",
  SUM("orders"."order_total") AS "order_revenue",
  COUNT(DISTINCT "orders"."order_id") AS "order_count"
FROM "main"."orders" AS "orders"
  LEFT JOIN "main"."customers" AS "customers" ON "orders"."customer_id" = "customers"."customer_id"
GROUP BY 1, 2
LIMIT 20;
```

And what it will not do:

```
$ truegrain compile -models ./models -metric order_revenue -dim order_lines.item_id

refused (fan_out_would_inflate): metric "order_revenue" aggregates SUM(...) over
orders, but this query repeats orders rows: order_lines is not unique on
order_id, so joining it repeats every orders row once per matching order_lines row
  hint: the total would be inflated, so the engine will not run it; drop the
  dimensions or metrics that pull in relationship "lines_to_orders", or use an
  aggregate that survives duplication such as COUNT(DISTINCT ...), MIN or MAX
```

On the fixture data that query would have answered **2361.00** instead of
**885.50**, an overstatement of 167% that looks entirely plausible. Silently
returning it is the failure mode that kills trust in a semantic layer, so the
engine refuses instead.

## Install

```bash
# A single static binary, no runtime and no toolchain needed
curl -fsSL https://github.com/rk-chavali/truegrain/releases/latest/download/truegrain_linux_amd64.tar.gz | tar xz

# Or a container, against BigQuery
docker run --rm -v "$PWD:/models" ghcr.io/rk-chavali/truegrain:latest validate -models /models
```

Every release carries `checksums.txt` and a build attestation, so a download
can be verified back to the commit and the workflow that produced it:

```bash
gh attestation verify truegrain_linux_amd64.tar.gz --repo rk-chavali/truegrain
truegrain version    # says which commit it came from
```

The container deliberately does not bundle the DuckDB CLI. DuckDB is the local
quickstart below; a container is what runs against a real warehouse.

## Quickstart

No cloud account, no credentials, no C compiler.

```bash
make build
make validate
make compile
```

To run real queries, put the [DuckDB CLI](https://duckdb.org/docs/installation/)
on your PATH (a single executable) and:

```bash
make demo
```

## Start from your warehouse

The quickstart runs on a model that ships with the repository. To get one for
your own data, `truegrain init` reads what the warehouse already knows:

```bash
truegrain init
```

```
Warehouse [bigquery]:
BigQuery project [acme-analytics]:
Dataset: main

Reading acme-analytics.main

  campaigns             4 columns   key: campaign_id
  customers             4 columns   key: customer_id
  order_lines           5 columns   key: order_id, line_number
  orders                5 columns   key: order_id

  3 relationship(s) from declared foreign keys
    campaigns.order_id -> orders.order_id
    order_lines.order_id -> orders.order_id
    orders.customer_id -> customers.customer_id

  wrote  truegrain.yaml
  wrote  models/main/namespace.yaml
  wrote  models/main/datasets.generated.yaml
  wrote  models/main/metrics.yaml
```

Every flag can be given instead of answered, which is how to run it
unattended:

```bash
truegrain init -dialect bigquery -project acme-analytics -dataset main -location US
```

**It reads no rows.** Tables, columns, types, primary keys and foreign keys,
through the metadata API. So it needs `roles/bigquery.metadataViewer` and
nothing else: no ability to run a query, no access to a single value. That is a
credential somebody will give a tool they are still deciding about.

**The keys are the point.** Columns alone produce a model that compiles and
cannot tell that joining `order_lines` multiplies `orders` rows. Grain comes
from the primary key and joins come from the foreign keys, so a table that
declares neither is named in the output and again in the generated file:

```
! events declares no primary key, so its grain cannot be derived and a
  fan-out through it cannot be detected
```

Nothing is guessed. A guessed key is worse than a missing one, because the
engine would believe it knows the grain, pass the fan-out check, and inflate a
sum with complete confidence.

**Metrics are not generated.** A schema can say an order has a total. Only you
can say which of those totals the business calls revenue, and a tool that
guesses produces a catalogue of plausible metrics nobody trusts. `metrics.yaml`
arrives with one commented example built from a real column in your model, and
`init` never writes that file twice.

Re-run it after a schema change with `-force`. That rewrites
`datasets.generated.yaml` and leaves your metrics, your namespace manifest and
your `truegrain.yaml` alone.

Then the loop the rest of this README describes:

```bash
truegrain validate -models .    # does the model hold together
truegrain test     -models .    # does it answer, and refuse, what it should
truegrain doctor   -models .    # does the warehouse still agree with it
truegrain query    -models . -metric main.order_revenue -dim main.customers.region
```

## Already have a semantic layer

```bash
dbt parse                                    # writes target/manifest.json
truegrain import -from dbt    -manifest target/manifest.json -dialect postgres
truegrain import -from lookml -project ./my-looker-project   -dialect bigquery
truegrain import -from cube   -project ./model               -dialect postgres
```

No warehouse credentials for any of them: each reads files.

Each source already declares what fan-out detection needs. dbt's MetricFlow
entities are the grain and the join graph; Looker and Cube state join
cardinality directly, so `one_to_many` arrives as a fan-out nobody had to
infer. An imported project therefore starts refusing questions the source
answered. That is the point, and it is worth being ready for:
**importing can reveal that a number somebody has been reading for a year is
inflated.** The engine refuses it rather than reproducing it, and names the
metric that answers correctly.

What does not come across is printed in full, not counted: cumulative and
conversion metrics are window functions this engine refuses by name, a
derived metric can compose metrics at different grains, and a semi-additive
measure is the thing the whole project exists to not guess at. Each is
reported with the reason, because a catalogue quietly missing a third of its
metrics is worse than one that refused to import.

## Assert what the model answers

`validate` says the model holds together and `diff` says a number changed.
Neither says a number was ever right. `truegrain test` is where you say so.

```yaml
# tests/retail.yaml
version: 1
tests:
  - name: total revenue is 885.50
    metrics: [order_revenue]
    expect_rows:
      - [885.50]

  - name: revenue by item is refused, because it would inflate
    metrics: [order_revenue]
    dimensions: [order_lines.item_id]
    expect_refusal: fan_out_would_inflate
```

```bash
truegrain test -models . -tests ./tests
```

The second case is the one worth copying, and it is the one no other semantic
layer can express. **A refusal assertion needs no warehouse**: the planner
declines a fan-out before any SQL exists, so it runs in a pull request with no
credentials configured, alongside `validate` and `diff`. What a model refuses
is the most valuable thing to test about it and the cheapest thing to check.

Row assertions need a warehouse. A run without one reports how many cases it
could not check rather than printing a clean pass over work it did not do, and
`-fail-on-skip` turns that into a failure for a pipeline that believes it is
checking rows. `-json` emits a report for a CI step to read.

Numbers compare numerically, so `750`, `"750.00"` and `750.0` all match: which
of those arrives depends on the warehouse driver, not on the model. `tolerance`
exists for division, which genuinely differs between warehouses, and nothing
else should need it.

Today `init` reads BigQuery and PostgreSQL. For Postgres, name the variable
holding the connection string rather than passing it:

```bash
export DATABASE_URL=postgres://reader@localhost:5432/warehouse?sslmode=require
truegrain init -dialect postgres -dsn-env DATABASE_URL -dataset public
```

It reads `pg_catalog` only, never a row, so the credential it needs is CONNECT
plus USAGE on the schema. DuckDB and Snowflake have no reader: those models are
written by hand and then queried like any other. See
[what is not built](#not-built-yet).

## Try it in a browser

The console is a separate React application at
[truegrain-console](https://github.com/rk-chavali/truegrain-console). It is a
static build you host yourself: explore the model, read what is and is not
enforced, and copy the client code for a query you just ran.

```bash
truegrain serve rest -models ./models -db ./demo.duckdb   -identity you@example.com -token-env SEMANTIC_TOKEN   -cors-origin http://localhost:5180
```

`-cors-origin` names the origins a browser client may read responses from. It
is off by default and never accepts a wildcard: on an engine running without
authentication, which is the normal local configuration, a wildcard would let
any website you happen to visit read your entire model.

## Non-negotiables

These shape every other decision in the codebase.

1. **No `run_sql` escape hatch on any interface.** The only expressible request
   is a semantic one. `TestNoRawSQLTool` and `TestNoEndpointAcceptsSQL` guard it.
2. **Governance resolves at compile time.** An unauthorized plan never becomes
   SQL. `TestDeniedProducesNoSQL` asserts the absence of a compiled statement,
   not just the presence of an error.
3. **Never a shared admin service account.** Identity is passed to the executor
   on every call so queries run as the caller.
4. **The model format is Ossie.** No proprietary YAML dialect. The engine reads
   the published `tpcds_semantic_model.yaml` from the Apache Ossie repository
   unmodified; `TestUpstreamExamples` proves it on every run.
5. **Correctness is proven in CI**, not claimed here.

## More than one team

A directory of Ossie files is either one model, in which case every team shares
one flat namespace and collides, or several models, which most loaders refuse.
Neither works once two teams own definitions. So each team's directory carries
its own manifest:

```
domains/
  sales/
    namespace.yaml        # name, owners, exports
    sales.yaml            # stock Ossie, unmodified, portable
  marketing/
    namespace.yaml        # name, owners, imports
    campaigns.yaml
```

Nothing is shared, so there is no root file for twelve teams to contend on.
Adding a team is a new directory.

```yaml
# domains/sales/namespace.yaml
version: 1
name: sales
owners: ["@acme/sales-ops"]
exports:
  datasets: [orders]      # private by default; this is the whole public surface
```

```yaml
# domains/marketing/namespace.yaml
version: 1
name: marketing
owners: ["@acme/growth"]
imports:
  - namespace: sales
    datasets: [orders]    # both sides opt in
```

**Bare names inside a namespace, qualified names outside.** Marketing's model
file writes `SUM(orders.order_total)` and stays a valid Ossie document any
other OSI tool can read. The API returns `marketing.attributed_revenue`, and
accepts a bare name when it is unambiguous. All composition metadata lives in
our manifests, never in the customer's models.

```bash
make teams
```

Three properties that suite demonstrates:

- **Composition works.** Marketing aggregates a dataset sales exported, joined
  across the namespace boundary.
- **Metrics from two namespaces combine.** Each is aggregated separately and the
  results are joined on a dimension both can reach. See below.
- **A grant follows its column.** The revenue tag is written by sales against
  `sales.orders.order_total`. Marketing reads that column through the import and
  is denied by the same grant. Governance resolves against where a field was
  defined, never the name it is used by, so aliasing an import is not a bypass.

One team cannot take the layer down for everyone: `validate` fails hard on any
namespace error, and `serve` loads what is valid, marks the rest unavailable in
`health`, and refuses queries touching them with a named reason.

## Facts at different grains

"Revenue and marketing spend by month" asks for two facts that live at different
grains. One flat join cannot answer it: joining orders to campaigns, or an order
header to its lines, repeats one side and inflates any sum over it.

The engine aggregates each fact separately at the shared dimension grain and
joins the grouped results, which is the fix `docs/03-semantic-model.md`
prescribes for the chasm trap.

```sql
WITH "fact_1" AS (
  SELECT date_trunc('month', "orders"."order_date") AS "order_date",
         SUM("orders"."order_total") AS "order_revenue"
  FROM "main"."orders" AS "orders" GROUP BY 1
),
"fact_2" AS (
  SELECT date_trunc('month', "orders"."order_date") AS "order_date",
         SUM("campaigns"."spend") AS "campaign_spend"
  FROM "main"."campaigns" AS "campaigns"
    LEFT JOIN "main"."orders" AS "orders" ON "campaigns"."order_id" = "orders"."order_id"
  GROUP BY 1
)
SELECT COALESCE("fact_1"."order_date", "fact_2"."order_date") AS "order_date",
       "fact_1"."order_revenue", "fact_2"."campaign_spend"
FROM "fact_1"
  FULL OUTER JOIN "fact_2" ON "fact_1"."order_date" IS NOT DISTINCT FROM "fact_2"."order_date"
```

Three details that are not incidental. The join is **full outer**, because a
month with orders and no campaigns is a real row and an inner join would
understate it. The condition is **null safe**, because a null dimension value is
a real group. And the dimension is **coalesced** across parts rather than read
from the first, for the same reason.

Parts may come from different namespaces. Two parts share a dimension when their
fields have the same governed origin, which is the identity an imported dataset
preserves.

The governance gate runs over **every** part before any of them is emitted.
Checking only the first would let every later metric escape it.

### What is still refused, and why

A sum over the order header grouped by a product dimension:

```
refused (fan_out_would_inflate): metric "order_revenue" aggregates SUM(...) over
orders, but this query repeats orders rows
  hint: the total would be inflated, so the engine will not run it. Metrics
  defined at the order_lines grain answer this correctly: line_revenue,
  units_sold. ...
```

That one is not a limitation. An order contains several products, so "order
revenue for hardware" has no answer without an allocation rule, and summing it
across categories would exceed the real total. The refusal names the metrics
defined at the grain where the question *is* well defined.

Likewise, a namespace that has not imported a dataset cannot group by it, and
the refusal says exactly which file to edit:

```
refused (cross_namespace_query): namespace "marketing" cannot group by
"sales.customers.region", which belongs to "sales"
  hint: add the dataset to `imports` in marketing's namespace.yaml, and to
  `exports` in sales's, or drop this dimension
```

## Answering from a pre-aggregated table

A `rollups.yaml` beside the model declares which pre-aggregated tables
exist, what each holds, and how each proves it is current:

```yaml
version: 1
rollups:
  - name: orders_by_region_month
    source: main.rollup_orders_daily
    dimensions:
      - {field: customers.region,   column: region}
      - {field: orders.order_date,  column: order_month, grain: month}
    metrics:
      - {metric: order_revenue, column: order_revenue}
    freshness:
      column: built_at
      max_age: 26h
```

A question the planner can answer from one is; a question it cannot is
answered from the source tables. Nothing is ever refused because of a
rollup.

A rollup is a second copy of a number, which is the thing this engine
exists not to do quietly, so every rule is a reason not to route:

- **A freshness check is required.** No `column` and `max_age`, no load.
  The engine asks the warehouse for `MAX(column)` before reading the table
  and falls back when it is stale. An engine with no executor never routes,
  because compiled output must not depend on state nothing verified.
- **Only distributive metrics.** SUM, COUNT, MIN and MAX can be recomputed
  from partial aggregates. `COUNT(DISTINCT ...)`, `AVG` and any ratio
  cannot, and listing one fails the load.
- **The recombining function is derived, not declared.** A stored COUNT
  recombines with **SUM**, because counting the rollup's rows counts groups
  and COUNT there returns a smaller number in the right units that nothing
  would flag. There is nowhere in the file to get this wrong.
- **Governance is unchanged.** The plan is built against the model and only
  then rewritten, so a grant written on `customers.region` keeps applying
  when a rollup carries a copy of that column.

Every answer that came from one says so, on the response, in the audit log
and on `GET /v1/health`, which also reports when each rollup was last
probed and why one is not being used. Details in
[docs/03-semantic-model.md](docs/03-semantic-model.md).

## Interfaces

Five surfaces, one planner, one request vocabulary.

| Surface | Command |
|---|---|
| CLI | `truegrain validate \| test \| compile \| query \| diff \| health` |
| MCP (stdio or streamable HTTP) | `truegrain serve mcp` |
| REST / JSON | `truegrain serve rest` |
| PostgreSQL wire protocol, for BI tools | `truegrain serve postgres` |
| GraphQL, for a front end that speaks it | `POST /graphql` |

A BI tool connects to `serve postgres` as it would to any PostgreSQL. What it
sends is parsed into a semantic request and never forwarded as text, so the
governance gate, the fan-out refusal and the audit record all still apply: a
question that would inflate a total comes back as an `ERROR` whose `HINT`
names the metric that answers it correctly.

The MCP tool surface is fixed at four tools: `list_metrics`,
`describe_metric`, `list_dimensions`, `query`. There is no fifth, and adding one
that accepts SQL fails the test suite.

### Queries that outlive the caller

A warehouse query can run longer than the default HTTP timeout of every agent
framework in use. Synchronously the client gives up, retries, and the warehouse
runs and bills the query twice for an answer nobody reads.

```python
result = client.run(metrics=["sales.order_revenue"], dimensions=["sales.customers.region"])
```

`run` submits, polls with backoff, collects every page and returns one result,
so no caller writes a polling loop. Underneath it is `POST /v1/jobs`, `GET
/v1/jobs/{id}` and `DELETE /v1/jobs/{id}`.

Three properties are not incidental:

- **Compilation is synchronous**, so the governance gate runs before the job
  exists. A request you may not make is refused on submit, not discovered by
  polling. A 202 is a promise the query was authorized.
- **A job is readable only by the identity that submitted it.** Another caller
  gets 404 rather than 403, so job identifiers cannot be probed for existence.
- **Abandoning a wait cancels the job**, rather than leaving the warehouse
  spending on an answer nobody is waiting for.

MCP deliberately does not get this. An MCP client is a local process over stdio
with no request timeout to outlive, so a fifth tool would be cost with no
benefit.

### Connecting Claude Code

```bash
claude mcp add truegrain -- /path/to/truegrain serve mcp -models /path/to/models -db /path/to/demo.duckdb
```

## Pointing it at BigQuery

```bash
export SEMANTIC_TOKEN=...   # or use a real authenticator, see below
truegrain serve rest \
  -models ./models -policy ./policy.yaml \
  -dialect bigquery -project my-project -location US \
  -impersonate -token-env SEMANTIC_TOKEN
```

Credentials are Application Default Credentials and nothing else. There is no
key file path in any config field, because a key path in a project file becomes
a key file in a git repository.

Two behaviours are worth knowing before the first invoice.

**Every query is estimated before it runs.** A dry run reports the bytes the
statement would scan, and anything over the cap is refused rather than run:

```
refused (query_scans_too_much): this query would scan 1.4 TiB, over the
200.0 GiB limit
  hint: narrow it with a filter on the partitioning column, use a coarser
  grain, or raise the limit deliberately with -max-bytes-billed
```

The refusal classifies as `modify`, so an agent adds a filter instead of giving
up. `MaximumBytesBilled` is also set on the real job as a backstop, because the
estimate is not exact for clustered tables and materialized views.

**`-impersonate` runs each query as the calling service account**, which is what
makes the warehouse's own row and column security apply to the caller rather
than to this process. It needs `roles/iam.serviceAccountTokenCreator` on the
identities it acts as. Without it the engine still works and `health` says so in
full:

```
executor  bigquery (my-project); impersonation is off, so every query runs as
          this process's own service account and the warehouse cannot tell
          callers apart; capped at 200.0 GiB scanned per query
```

Use `-dry-run` to estimate every query and execute none, which is how to point
the engine at a real warehouse and validate a model against real schemas
without spending anything.

## Which commands need warehouse credentials

Most of them do not, and that is worth knowing before wiring anything into a
pipeline. A continuous integration job that checks a model should not hold a
credential that can read your customers.

| Command | Warehouse access | BigQuery role |
|---|---|---|
| `validate` | none | none |
| `diff` | none | none |
| `compile` | none | none |
| `init` | metadata: names, types, keys | `roles/bigquery.metadataViewer` |
| `doctor` | metadata: names and types | `roles/bigquery.metadataViewer` |
| `query`, `serve` | reads rows, runs jobs | `roles/bigquery.jobUser` plus `roles/bigquery.dataViewer` |

On Postgres the same split is a database role with `SELECT` on the tables the
model reads, and nothing else. Every query runs in a read-only transaction, so
the server refuses a write even if the emitter ever produced one.

`validate`, `diff` and `compile` read YAML and emit SQL. They never open a
connection, which is why the pull request check below runs with no cloud
credential at all.

`init` and `doctor` read metadata through the API. No query job runs, so they
cost nothing and cannot read a value.

### Three identities, not one

A pipeline that uses one service account for everything gives its weakest job
its strongest permissions. Split them by what each actually does:

```bash
# 1. CI: checks the model against the warehouse's shape. Metadata only.
gcloud iam service-accounts create truegrain-ci \
  --display-name "truegrain model checks"

gcloud projects add-iam-policy-binding acme-analytics \
  --member "serviceAccount:truegrain-ci@acme-analytics.iam.gserviceaccount.com" \
  --role roles/bigquery.metadataViewer

# 2. The engine: runs queries. The only identity that reads a row.
gcloud iam service-accounts create truegrain-engine \
  --display-name "truegrain query engine"

gcloud projects add-iam-policy-binding acme-analytics \
  --member "serviceAccount:truegrain-engine@acme-analytics.iam.gserviceaccount.com" \
  --role roles/bigquery.jobUser

# Grant data access per dataset, not project-wide.
bq add-iam-policy-binding \
  --member "serviceAccount:truegrain-engine@acme-analytics.iam.gserviceaccount.com" \
  --role roles/bigquery.dataViewer \
  acme-analytics:main
```

The third identity is not a service account. It is the API token a pipeline
uses to call a **running** engine, and it is narrowed with scopes rather than
IAM:

```bash
truegrain serve rest -models ./models -token-env CI_TOKEN -token-scopes read:model
```

That credential can list metrics, describe them and compile SQL. It cannot make
the warehouse do work. See [A token that cannot read your
data](#a-token-that-cannot-read-your-data).

Add `roles/iam.serviceAccountTokenCreator` to the engine's identity only if you
run it with `-impersonate`, which makes each query execute as the caller so the
warehouse's own row and column security applies to them rather than to the
engine.

### No key files in CI

Workload Identity Federation lets a GitHub Actions run authenticate to Google
directly. Nothing is downloaded, nothing is stored in a repository secret, and
there is no key to rotate or leak:

```bash
gcloud iam workload-identity-pools create github \
  --location global --display-name "GitHub Actions"

gcloud iam workload-identity-pools providers create-oidc github \
  --location global --workload-identity-pool github \
  --issuer-uri "https://token.actions.githubusercontent.com" \
  --attribute-mapping "google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --attribute-condition "assertion.repository == 'acme/analytics-models'"

gcloud iam service-accounts add-iam-policy-binding \
  truegrain-ci@acme-analytics.iam.gserviceaccount.com \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/github/attribute.repository/acme/analytics-models"
```

The `--attribute-condition` is the part worth getting right. Without it, any
repository on GitHub can mint a token for this service account.

```yaml
name: Model
on: [pull_request, schedule]

permissions:
  contents: read
  id-token: write        # required to mint the OIDC token
  pull-requests: write

jobs:
  # No credential. Nothing here talks to a warehouse.
  meaning:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: rk-chavali/truegrain@v0.3.0
        with:
          models: .
          against: ${{ github.base_ref }}

  # Metadata only, as truegrain-ci. Cannot read a row.
  drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/github/providers/github
          service_account: truegrain-ci@acme-analytics.iam.gserviceaccount.com
      - run: truegrain doctor -models . -dialect bigquery -project acme-analytics
```

`doctor` exits 5 when the warehouse disagrees with the model, so the schedule
finds drift by looking rather than by a caller hitting it.

Everywhere else, credentials are Application Default Credentials and nothing
else: `gcloud auth application-default login` on a laptop, the attached service
account on Cloud Run or GKE, `GOOGLE_APPLICATION_CREDENTIALS` where you must.
There is no key file path in any config field, because a key path in a project
file becomes a key file in a git repository.

## Who is calling

Everything this engine promises about governance rests on the identity being
the caller's. Three authenticators ship, and they narrow as they get weaker.

```bash
# OpenID Connect: Okta, Entra, Auth0, Keycloak, Google
truegrain serve rest -oidc-issuer https://acme.okta.com/oauth2/default                     -oidc-audience semantic-prod -groups-claim groups

# Google-signed identity tokens: Cloud Run, GKE, Compute Engine
truegrain serve rest -google-audience https://semantic.acme.internal

# One shared token: a single tenant, and nothing more
truegrain serve rest -token-env SEMANTIC_TOKEN
```

Verification is delegated to `go-oidc` and Google's own verifier rather than
hand written. Signature checking, discovery and key rotation are where JWT
authentication goes wrong, and a bespoke parser is the most common way to end
up accepting a forged token.

Four things fail closed rather than defaulting to permissive:

- **An audience is mandatory.** Without one, every token the issuer ever minted
  for any application would authenticate here. That is the most expensive
  common OIDC misconfiguration, so the server refuses to start instead.
- **Only asymmetric algorithms are accepted.** With HMAC in the list a caller
  can sign a token using the provider's public key as the shared secret. `alg:
  none` is rejected for the obvious reason.
- **A plaintext issuer is refused**, because keys fetched over HTTP can be
  substituted in transit and the verification becomes decorative.
- **An unverified `email` claim is not an identity.** It is caller-supplied, and
  the subject is exactly what the policy file and the warehouse grant key on.

Serving on anything but loopback without authentication is refused outright. A
static token authenticates but maps every caller to one subject, so the audit
log cannot tell them apart; the server says so on a non-loopback bind.

What none of this defends against: a stolen token works until it expires. Serve
over TLS and keep lifetimes short.

## Clients

The SDKs live in their own repositories, so a Java contributor does not clone Go
to fix a Python bug and each one releases on its own cadence.

| Language | Install | Repository | Reference |
|---|---|---|---|
| Python | `pip install truegrain` | [truegrain-python](https://github.com/rk-chavali/truegrain-python) | [pages](https://rk-chavali.github.io/truegrain-python/truegrain.html) |
| TypeScript | `npm install truegrain` | [truegrain-typescript](https://github.com/rk-chavali/truegrain-typescript) | [pages](https://rk-chavali.github.io/truegrain-typescript/) |
| Go | `go get github.com/rk-chavali/truegrain-go` | [truegrain-go](https://github.com/rk-chavali/truegrain-go) | [pkg.go.dev](https://pkg.go.dev/github.com/rk-chavali/truegrain-go) |

They are hand-written rather than generated. A generator produces a transport
and a flat error type; the parts worth having are the ones it cannot produce,
namely the refusal semantics an agent branches on and a `run()` that submits a
job and polls it so no caller writes that loop.

Each client vendors `api/openapi.yaml` and checks itself against it in both
directions, so an operation this engine gains that a client cannot call is a
failing build in that client rather than a discovery made in production.

## A token that cannot read your data

One bearer token used to grant everything, so a continuous integration job
that only validated a model held a credential that could also read every row
its identity was allowed.

```bash
truegrain serve rest -models ./models -token-env CI_TOKEN -token-scopes read:model
```

That credential lists metrics, describes them and compiles SQL. It cannot
execute anything:

```
code:   out_of_scope
reason: this credential is not permitted to run queries against the warehouse
hint:   it holds read:model; use a credential with run:query, or ask an
        operator to widen this one
retry:  never
```

Two scopes, `read:model` and `run:query`, because the line worth drawing is
between reading what the model says and making the warehouse do work. That is
the line between a cheap secret and an expensive one.

Naming no scopes grants all of them, so nothing changes for a deployment that
had one token. Naming any makes the list exhaustive: `run:query` does not
imply `read:model`, because "it can already run queries so it may as well read
metadata" is how a scope system turns back into one token. A scope that is not
recognised fails at startup rather than being dropped, since a typo would
otherwise produce a credential narrower than intended and fail somewhere far
from the mistake.

## Governance

Column access is resolved between the planner and the emitter. A denied request
returns before any SQL exists: there is no compiled statement to leak and no
warehouse job to cancel.

```bash
truegrain health -models ./models -policy ./policy.yaml
```

`health` reports what the deployment actually enforces, including what it does
not. Overstating a governance guarantee is worse than not offering one.

Three resolvers ship today:

- **file** — a policy file next to the model. Real compile-time enforcement,
  provable on any machine with no cloud account. It governs queries that go
  through this engine and places no control on the warehouse itself.
- **allow-all** — for local work against fixture data. It says so in `health`.

- **bigquery-policy-tags** — the warehouse's own control. The tags are already
  on your columns and the caller's own IAM decides, so nothing is restated in
  our format and nothing drifts.

```bash
truegrain serve rest -dialect bigquery -project my-project -policy-tags -impersonate
```

A column with no policy tag is readable, which is what BigQuery itself does. A
tagged column requires `roles/datacatalog.fineGrainedReader`, and the check is
made **as the caller** rather than as this process. Evaluating the tag's IAM
policy locally would be wrong: it misses bindings inherited from the project,
folder and organization, conditional bindings, and group expansion, so it would
report access the warehouse refuses and refuse access it allows.

A field whose expression does not reduce to a single column is treated as
reading every tagged column in its table. That can deny a field which would
have been allowed. It can never allow one that should have been denied, which
is the direction to be wrong in.

`-policy-tags` and `-policy` are refused together. They are two sources of
truth for one decision, and silently preferring one would leave an operator
believing the other was in force.

This pairs with a modelling tool that writes the tags: classify a column once,
and the tag is enforced both in the warehouse and here, before the query is
sent.

## Does this change a number

A model edit that still validates, still passes every test, and quietly moves
every dashboard is the failure mode this project exists to prevent. Reviewing a
YAML diff does not catch it: the YAML change is small and the consequence is not.

`truegrain diff` compares compiled SQL rather than model text, so renaming a
description is invisible and changing a join, a grain or an expression is not.

```bash
git worktree add /tmp/base origin/main
truegrain diff -models . -against /tmp/base
```

```
Changed meaning (4). These compile differently than before:
  ~ sales.shipped_revenue
  ~ sales.shipped_revenue by sales.customers.region
  ...
```

Exit 0 when nothing changed, 4 when something did, so CI can gate on it. A
request that used to be refused and now compiles counts as a change, and so does
the reverse.

### In a pull request

The action runs the same comparison and leaves the answer where the review
happens. It edits one comment rather than adding a new one per push.

```yaml
name: Semantic model
on: pull_request

permissions:
  contents: read
  pull-requests: write

jobs:
  model:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: rk-chavali/truegrain@v0.3.0
        with:
          models: .
          against: ${{ github.base_ref }}
```

The comment it writes:

> **4 changed meaning.** This pull request changes what the model computes.
>
> **Changed meaning (4)** — these still compile. They compute something different.
> - `sales.shipped_revenue`
> - `sales.shipped_revenue by sales.customers.region`

Omit `against` to validate only. Add `fail-on-change: true` for a model nobody
should change without a conversation, and `full: true` to include the SQL on
both sides. It exposes `changed`, `removed`, `altered` and `added` as outputs,
so a later step can decide something.

Both commands also speak JSON, for a pipeline that is not GitHub Actions:

```bash
truegrain validate -models . -format json
truegrain diff -models . -against /tmp/base -format json
```

`validate` emits JSON on failure too, with one entry per problem, because the
run worth parsing is the one that failed.

## Is the model still true

`diff` compares a model against its previous self. `doctor` compares it against
the warehouse it claims to describe.

```bash
truegrain doctor -models . -dialect bigquery -project my-project
```

```
4 table(s) checked.

  ! sales.customers
      this table does not exist
      in main.customer_dim
      every query touching this dataset fails; fix the source or drop the dataset

  ! marketing.orders.order_total
      reads column "order_total_amount", which this table does not have
      in main.orders
      every query using this field fails

2 will break a query.
```

Metadata only: it reads no rows, so it needs no more than permission to list a
schema, and it is cheap enough to run on a schedule. Drift is found by looking
regularly rather than by a caller hitting it.

Exit 0 when the warehouse agrees, 5 when it does not, and `-format json` for a
pipeline. A type the warehouse contradicts is a warning rather than an error,
because the query still runs and only its meaning has moved.

An executor that cannot introspect says so rather than reporting the model as
healthy, which is the one answer worth never giving.

### Which rows, not just which columns

A tag decides whether a caller may read the email address. This decides whether
a regional manager may read another region's revenue, which until now was yes,
because nothing asked.

```yaml
rows:
  - namespace: sales
    principal: "@acme/northeast"
    description: The northeast team sees northeast orders only.
    filter:
      dimension: customers.region
      op: eq
      values: ["NE"]
```

```
$ truegrain query -metric sales.order_revenue -identity sam@acme.com -groups @acme/northeast
750.00

$ truegrain query -metric sales.order_revenue -identity chris@acme.com
885.50
```

The filter is added to the request before it is planned, so the planner and the
gate both see the narrowed query and no second path exists that could skip it.
It combines with the caller's own filters by AND, so narrowing a query can
never widen it, and there is no request field that names it, so it cannot be
removed. A filter whose dimension no longer resolves refuses the query rather
than being dropped: silently ignoring a restriction is a data leak wearing the
costume of a permissive default.

A policy restricts the principals it names and nobody else, which matches how
an untagged column is readable. `health` reports how many principals are
covered so a partial policy is visible rather than assumed complete, and does
not name them.

## Correctness

```bash
go test ./...          # everything that needs no database
make test-all          # adds the parity suite against real DuckDB
```

| Suite | What it proves |
|---|---|
| `internal/osi` | The expression parser handles every construct the Ossie spec marks REQUIRED, and the decomposability table matches the spec row for row |
| `internal/resolve` | The engine reads a published upstream Ossie model, and refuses an ontology document by name |
| `internal/plan` | Fan-out and chasm joins are refused; safe joins are not |
| `internal/dialect` | Committed golden SQL per dialect, so an emitter change is a readable diff. Filter values never appear in SQL text |
| `internal/engine` | The governance suite from `docs/06-validation.md`, minus the GCP-specific rows |
| `internal/exec` | Real SQL against real data: totals agree with and without a grouping dimension, and metrics match hand-computed answers |
| `internal/serve/*` | No interface exposes raw SQL; refusals carry an actionable code and hint |
| `internal/govern` (tags) | An untagged column is readable, every tag on a column must allow, a computed field requires every tag in its table, and a lookup failure is an error rather than a decision |
| `internal/govern` | Fail-closed on a policy outage, no cache leakage across identities or groups, a revoked grant takes effect, denials do not disclose the schema |
| `internal/serve/rest` (auth) | A missing audience and a plaintext issuer fail to start, only asymmetric algorithms are accepted, an unverified email is not an identity, and no error echoes the credential |
| `internal/exec` (BigQuery) | The spend cap refuses before the query runs, `health` states when impersonation is off, and bound parameters match the placeholders the emitter writes |
| `internal/config` | Environment values are required rather than defaulted to empty |
| `cmd/truegrain` | The built binary: exit codes, and that every command resolving access records the decision |
| `internal/rollup` | A rollup may only hold metrics that can be recomputed from partial aggregates, a stored COUNT recombines with SUM, and a rollup with no freshness check does not load |
| `internal/engine` (rollups) | Against real DuckDB: a rollup answers the same total as the fact tables, a stale one is never read, a grain finer than the rollup falls back, and governance still resolves against the model |
| `internal/serve/pgwire` | A catalogue statement is answered with the caller's own filter applied or declined outright, and a hostile name is refused rather than carried |
| `internal/govern` (rotation) | A rotation has no window where neither token works, an expired token stops working, and an unreadable file does not lock everybody out |
| `internal/config` (environments) | An overlay changes only what it mentions, and an unknown `-env` is an error rather than a fallback to the base configuration |

## Architecture

```
Ossie YAML → parser → resolver → planner → governance gate → emitter → executor
                                              ↓ denied
                                         structured refusal
```

`internal/plan` holds no SQL strings, so correctness is unit-testable without a
database. `internal/engine` is the only thing that owns a dialect, so no
interface can emit SQL without the gate having run.

The engine reads Apache Ossie, which carries no grain, additivity or cardinality
fields. All three are derived; `internal/osi/COMPLIANCE.md` records every
construct, its status, and each place this engine diverges from the spec.

## Not built yet

Stated plainly rather than implied by omission.

- **No executor has ever run Databricks, ClickHouse, Athena, Trino or
  Redshift.** All five dialects compile and their output is golden tested,
  and not one of them has a way to execute. Each says so in its own
  capabilities rather than leaving it to be found.
- **The Snowflake executor has never run against a real account.** It speaks
  the SQL REST API with a key-pair assertion, and everything about it that
  would be this engine's fault, the assertion, the bindings, the decimal
  scale, the partition paging, is tested. That Snowflake accepts what is
  sent is not, and `Name()` reports it as unverified.
- **A BigQuery round trip proven in CI.** The executor's refusal logic, spend
  cap and parameter binding are unit tested and the emitted SQL is golden
  tested, but no test in this repository runs a query against real BigQuery.
  DuckDB and Postgres both do, against the same fixture, on every run.
- **The Terraform fixture** for the policy tag resolver: two service
  accounts, a taxonomy and a tagged column, from `docs/06-validation.md`.
  The decision logic is unit tested against fakes; the Data Catalog round
  trip is not.
- **`truegrain init` for Snowflake and DuckDB.** It reads BigQuery and
  PostgreSQL. The shape it produces is warehouse-agnostic, so each remaining
  one is a reader rather than a redesign.
- **Importers for anything but dbt, LookML and Cube.** Those three are read.
  Apache Ossie is building converters upstream in `converters/`; check there
  before starting a fourth.
- **psql's `\d <table>` over the wire protocol.** It sends a join across
  half a dozen catalogue tables with regex operators. `\dt` works, and so
  does reading `information_schema.columns` directly. A statement the
  catalogue cannot answer is logged with the name the client gave itself,
  so an operator can see what their tool sent.
- **Cumulative and conversion metrics.** Window functions parse and are
  refused by name.
- **Semi-additive measures.** Not expressible in Ossie; refused, not
  guessed. This is also why a balance cannot go in a rollup.
- **Cancelling a query on another replica.** Jobs can be shared between
  replicas with `-job-store-dsn-env`, so submitting on one and polling
  another works. Cancelling still only stops the warehouse query on the
  replica running it: elsewhere the job is marked cancelled and the caller
  stops waiting, but the query runs to its own deadline. A
  `context.CancelFunc` is a pointer in one process's memory, and no store
  changes that. `GET /v1/health` says which store is configured and what it
  means.
- **Any write path.** There is no INSERT, no CREATE, no materialization.
  Rollups are read, never built: whatever builds them is your own pipeline,
  and the engine's only interest in it is the freshness column it writes.

## Licence

Apache 2.0, matching Apache Ossie.
