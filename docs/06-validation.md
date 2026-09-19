# 06 Validation

What is proven, how, and what is not. Rewritten on 17 September 2026: the
previous version described six suites in the future tense and then stopped
being updated, so it read as though all six ran.

A thin semantic layer that silently returns wrong numbers is worse than no
semantic layer. The failure mode that kills adoption is not a missing feature,
it is one number that disagrees with the dashboard and cannot be explained.

## Proven in CI, on every pull request

**Totals do not change when you add a dimension.** `TestTotalsAgreeWithAndWithoutADimension`
sums a measure with and without a child dimension in the group by and asserts
the totals match. This is the test that catches most real bugs.

**The engine agrees with hand-written SQL.** `TestKnownGoodAnswers` computes
each fixture metric with an independent query and asserts equality. Those
queries are the oracle and are reviewed as carefully as the engine.

**Header and line totals agree.** `TestHeaderAndLineTotalsMatch`. The fixture
is built so `order_revenue` and `line_revenue` both come to 885.50 by different
paths, which is what makes a fan-out visible when it happens.

**The refused query really would have been wrong.**
`TestFanOutWouldHaveBeenWrong` runs the query the engine declined and asserts
the number is inflated. A refusal nobody can show was necessary is a refusal
people will disable.

**Filters bind as parameters** and **month grain buckets correctly**.

**Compiled SQL is snapshotted for all four dialects.** Eleven golden files each
for DuckDB, BigQuery, Postgres and Snowflake. A change to any emitter shows up
as a reviewable diff.

**The published contract matches the server.** `TestOpenAPIMatchesRoutes`
asserts in both directions that `api/openapi.yaml` and the mux agree. It has
caught a missing endpoint twice.

**A pull request cannot silently change a number.** `truegrain diff` compiles
both sides and compares. See `README.md`.

All of the above runs against DuckDB, which needs no credentials.

### And against a real PostgreSQL

Added 18 September 2026 and **proven in CI on every run**, not by hand: the
engine job runs a `postgres:17` service container and the parity suite executes
the same fixture and the same metrics against it that it runs against DuckDB.
A service container rather than a skip, because the build fails on any skipped
test and a parity suite that quietly stops running is worse than one that was
never written.

| Check | Result |
|---|---|
| Six known-good answers | exact |
| Every metric against DuckDB | 8 of 10 agree exactly |
| The 2 that differ | division precision, see below |
| Fan-out | refused, at compile time |
| What the refusal prevented | **2361.00 against a true 885.50** |
| DECIMAL scale preserved | `75.50`, not `75.5` and not a fraction |
| A write through the executor | refused by the read-only transaction |

The same fixture file, unmodified, loads into PostgreSQL and DuckDB. If it ever
needs a per-warehouse variant then the two suites stop comparing the same data
and a parity result between them means nothing.

**The two differences are division precision, and will not be fixed.** Postgres
divides `NUMERIC` into `NUMERIC` and keeps the scale, reporting
`13.4166666666666667` and `177.1000000000000000`; DuckDB divides into a
`float64` and reports `13.416666666666666` and `177.1`. This is the same
finding already recorded for BigQuery above, arriving from a third warehouse:
sums, counts and maximums agree exactly, and a ratio carries the warehouse's
own division precision. The executor passes the warehouse's rendering through
rather than choosing a scale on its behalf.


## Proven by hand, once, on 17 September 2026

Against a real BigQuery project. Not automated, and worth repeating before any
release that touches the emitter or the executor.

The same model files used against DuckDB, unmodified, with the dataset named
`main` so `main.orders` resolves in both.

| Check | Result |
|---|---|
| Every metric, both warehouses | 18 of 20 agree exactly, 0 errors |
| The 2 that differ | the ratio metric only |
| Fan-out | refused, at compile time |
| What the refusal prevented | **2361 against a true 885.50** |
| Spend cap | refused on a real dry-run estimate |
| Untagged column | readable |
| Column with a real policy tag | refused |

**The ratio difference is not a bug and will not be fixed.** DuckDB divides
decimals into a float64 and reports `13.416666666666666`; BigQuery divides
`NUMERIC` into `NUMERIC` and reports `13.416666667`. The compiled SQL is
identical on both sides. They agree to nine decimal places and then diverge
because the two warehouses round a recurring decimal differently. Sums, counts,
maximums and averages agree exactly; a ratio carries the warehouse's own
division precision. Forcing a common scale would mean rewriting the model's
arithmetic, which is worse.

That exercise found three bugs, all in the seam between this engine and Google,
and none of them reachable by a mock. They are described in `git log` for
v0.2.0.

### `truegrain init`, against the same project

| Check | Result |
|---|---|
| Four tables read, with columns and types | correct |
| Primary keys, including a compound one | correct |
| Foreign keys, with their column pairing | 3 of 3 |
| Generated model passes `validate` | yes |
| Generated model passes `doctor` against the warehouse | yes |
| A metric written into it returns real rows | yes |
| A fan-out through the discovered join | refused |

The last two are the ones worth caring about. The join that refuses the
fan-out is a join `init` discovered, so the chain from a warehouse's own
foreign keys to a refusal holds end to end with nothing written by hand except
the metric.

This found one bug of the kind only a live run finds. The first version read
constraints by joining `KEY_COLUMN_USAGE` to `CONSTRAINT_COLUMN_USAGE` on the
constraint name, and reported a two-column primary key as
`order_id, order_id, line_number, line_number`: a fan-out inside the tool whose
purpose is detecting fan-outs. Every unit test passed. It is now read from
`tables.get`, which returns an ordered key and explicit column pairs.

## Not proven

**Fine-grained reader enforcement.** A policy tag is read from a real taxonomy
and a tagged column is refused, but the refusal comes from the engine being
unable to impersonate a human caller rather than from an IAM denial. Proving
the IAM path needs a service account holding `serviceAccountTokenCreator`, in
an organizational project. This is the central governance claim and it is the
one thing still unverified. Do not let anything imply otherwise.

**The spend cap's upper bound.** Refusal below the cap is proven with a
one-byte limit. The test project is a BigQuery sandbox with no billing, so
nothing bills and behaviour on a genuinely expensive query is untested.

**Snowflake, executed.** Golden SQL exists and nothing has ever run it. It is
an emitter that looks right. Postgres was in this position until 18 September
2026 and is now covered above.

**Anything at scale.** The fixture is five orders. Nothing here says what
happens on a partitioned table with a billion rows.

**More than one replica.** The job store is one in-memory map.

**The CI credentialing in `README.md`.** The split into three identities is
right about what each command reaches, and that part is verified: `validate`,
`diff` and `compile` open no connection, and `init` and `doctor` run no query
job. What has not been executed is the setup itself. The `gcloud` commands
creating the service accounts, the Workload Identity Federation pool and the
`principalSet` binding have never been run, because the test project is a
sandbox with no organization. They are the documented Google setup, not a
transcript. Treat the role names as correct and the exact command sequence as
unverified.

**`truegrain init` on anything but BigQuery and PostgreSQL.** There is no reader for the other
three dialects, so their models are written by hand.

**Snowflake cannot execute at all.** `internal/exec` holds three executors:
BigQuery, DuckDB and Postgres. Snowflake emits golden-tested SQL that nothing
has ever run.

Until 18 September 2026 this was worse than a gap for both. `executor`
special-cased BigQuery and fell through to DuckDB, so
`-dialect postgres -db demo.duckdb` compiled Postgres SQL, executed it against
a DuckDB file, and printed "dialect postgres" over the answer. It returned the
correct number because that statement was portable. Now refused, with a test.

**`truegrain init` on a large or awkward schema.** The live run was four
tables, all flat, all with declared constraints. Handled in code and covered by
unit tests but never met by a real warehouse: `RECORD` and repeated columns
(left out, with a note, because a struct is not a dimension), views (read,
keyless), a foreign key pointing at another dataset (skipped), and a
description containing something that breaks YAML. Untested entirely: external
tables, and a dataset with thousands of tables, where one `tables.get` per
table stops being free.

## No Terraform fixture

The earlier version of this document promised a Terraform module that creates
the datasets, taxonomies, policy tags and two service accounts, so anyone could
reproduce the governance suite. It does not exist. Until it does, the GCP
validation above is something one person did once rather than something the
project can prove on demand, and that is the honest description of it.

## Interface conformance

`TestOpenAPIMatchesRoutes` in the engine, plus a vendored copy of the spec in
each client repository with a scheduled job that opens a pull request when the
pin drifts. Each client has its own test asserting it covers every operation
the spec declares, so a client cannot fall behind the engine unnoticed.

## The demo worth showing

Ask for revenue by region: get 885.50 split two ways. Ask for revenue by line
item: get refused, with the reason naming the relationship that would repeat
the rows and a hint naming `line_revenue` and `units_sold`, which answer the
same question correctly. Then run the refused query by hand and watch 2361 come
back.

That sequence is the product. It works on DuckDB with no credentials, and it
has been run against BigQuery.
