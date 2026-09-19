# 03 Semantic model

What the engine reads, and the modeling rules it enforces. This doubles as the
review checklist for any model written against it.

## Layer order

Metrics are the last of seven layers, not the first. Build and validate in this
order.

### 1. Sources

- One source names exactly one table or one SQL block.
- Every source declares its **grain**: what one row means. "One row per order
  line per day" is a grain. "Orders" is not.
- Sources point at curated tables, never raw landing tables.

### 2. Entities

The join surface. Without these the planner cannot resolve a join path.

- Each source declares a primary entity matching its grain.
- Foreign key columns are declared as foreign entities, not dimensions.
- Shared entity names must match exactly across sources or the link is not made.
- One key convention, natural or surrogate, applied everywhere.

### 3. Dimensions

- Categorical dimensions: low cardinality, stable values.
- Every source with measures declares a primary time dimension with an explicit
  base granularity.
- Supported rollup granularities are declared, not inferred.
- Fiscal calendars are a date-spine dimension table, never a CASE expression
  inside a metric.
- Store UTC, convert at the dimension, document which zone defines the business
  day.

### 4. Measures

Raw aggregations over one column. Not business definitions.

- One aggregation function per measure.
- Each measure references the time dimension it aggregates over.
- **Additivity is derived, not declared.**

> **Superseded.** This section originally required additivity to be declared on
> every measure, and made its absence a model error. Apache Ossie has no such
> field, so requiring one would have meant models authored for this engine
> specifically, which is a proprietary dialect wearing an Ossie hat and breaks
> non-negotiable 4 in `README.md`.
>
> The engine instead derives additivity from the aggregate function the author
> already wrote, using the **Decomposability Reference** in the spec's own
> `expression_language.md`: SUM, COUNT, MIN and MAX are distributive; AVG,
> STDDEV and VARIANCE are algebraic; MEDIAN, PERCENTILE and `COUNT(DISTINCT)`
> are holistic; the `APPROX_` family is sketch-based. A stock Ossie model works
> unmodified, and an author cannot get the declaration wrong because there is
> no declaration.
>
> The planner then asks a narrower question than decomposability, and it is the
> one that actually decides a refusal: does this aggregate survive having its
> input rows duplicated by a join? Only MIN, MAX, any `DISTINCT` form and
> `APPROX_COUNT_DISTINCT` do.
>
> **Semi-additive measures** (balances, inventory on hand) are the one concept
> this derivation cannot recover, because nothing in the expression
> distinguishes them from an ordinary sum. They are refused rather than guessed.
>
> See `internal/osi/COMPLIANCE.md`.

### 5. Metrics

- `simple`: a measure, optionally filtered
- `ratio`: numerator over denominator, computed after aggregation, never row by
  row
- `derived`: arithmetic over other metrics
- `cumulative`: running totals, rolling windows, period-to-date (v2)
- `conversion`: base event to converted event within a window (v2)

Rules:

- A metric name is a contract. Renaming is a breaking change and goes through
  the RFC process in `08-contributing.md`.
- Every metric has an owner and a plain-language description. For agent
  consumers the description is not documentation, it is the grounding text the
  model reads to decide whether the metric answers the question.
- Synonyms live in metadata so "top line", "net sales" and "revenue" resolve to
  one definition.
- No metric is defined twice. If a BI tool also computes it, one of them is
  deleted.

### 6. Joins and cardinality

- Every join declares cardinality.
- **Fan-out**: joining a fact to a one-to-many child duplicates parent rows, so
  any sum over the parent inflates. The planner aggregates before joining,
  applies symmetric aggregates, or refuses.
- **Chasm**: two unrelated facts joined through a shared dimension produces a
  cross product. Aggregate each fact separately, then join on the shared grain.
- Ambiguous paths are resolved explicitly in the model or rejected at compile
  time.

### 7. Governance annotations

Sensitivity and access requirements attached to dimensions and measures. See
`05-governance.md`.

## v1 OSI subset

Supported:

- datasets, with grain **derived** from `primary_key` rather than declared
- primary and foreign keys
- categorical and time dimensions with granularity
- metrics as expressions, with additivity **derived** from the aggregate
- simple and ratio metrics
- relationships, with cardinality **derived** from the keys involved
- descriptions, owners, synonyms
- workspace composition: several namespaces, with explicit imports and exports

Three of those say derived rather than declared, and that is the single most
important thing to understand about this engine. **Ossie declares no grain, no
cardinality and no additivity.** All three are inferred from `primary_key` and
the relationships. Fan-out detection is only possible because of that
inference: without it there is nothing to compare a metric's grain against.

Unsupported in v1, must produce an explicit unsupported error:

- cumulative and conversion metrics, which is to say window functions. These
  parse into the AST rather than being dropped, so the refusal can name the
  construct.
- nested or hierarchical dimensions
- subqueries inside an expression
- semi-additive measures, per the note above

> **Superseded.** This list originally also excluded "custom SQL expressions
> that bypass the planner". That rule would reject every conforming Ossie model.
> In Ossie a metric *is* a SQL expression string, so the engine parses the
> expression rather than refusing it: that is where the datasets a metric
> touches and the aggregate it uses are recorded, and a YAML parser alone cannot
> plan.
>
> Scalar functions outside the spec's own tables are passed through to the
> warehouse rather than rejected, because the spec's function list is explicitly
> extensible per dialect. The cost is that a typo in a function name surfaces as
> a warehouse error rather than a validate-time one.

> **Superseded.** The original "multi-hop entity chains beyond two joins" line
> described a cap that does not constrain real models the way it sounds. The cap
> counts hops *from the query's base dataset*, and a star or snowflake schema
> fans out from the base as a tree, so two hops reaches the whole thing. Facts at
> different grains, and metrics from different namespaces, are handled by
> aggregating each separately and joining on the shared dimensions rather than by
> a longer join path. See "Joins and cardinality" below and `README.md`.

Track OSI spec drift in a single file, `internal/osi/COMPLIANCE.md`, listing
every spec construct and its status: supported, unsupported, or intentionally
diverged with a reason.

## Rollups

A pre-aggregated table, declared in a `rollups.yaml` beside the model. A
request the planner can answer from one is; a request it cannot is
answered from the source tables.

```yaml
version: 1
rollups:
  - name: orders_by_region_month
    source: main.rollup_orders_daily
    dimensions:
      - field: customers.region
        column: region
      - field: orders.order_date
        column: order_month
        grain: month
    metrics:
      - metric: order_revenue
        column: order_revenue
    freshness:
      column: built_at
      max_age: 26h
```

A sidecar rather than a block in the model, because the Ossie spec has no
place for one and inventing a proprietary key would mean models had to be
authored for this engine. A sidecar rather than the project file, because
a rollup is a fact about the model: dev and prod read the same ones.

A rollup is a second copy of a number, which is the exact thing this
engine exists not to do quietly. So every rule below is a reason not to
route, and the fallback is always the source tables.

**Freshness is required.** A rollup with no `column` and `max_age` does
not load. Before reading one the engine asks the warehouse for
`MAX(column)` and compares it to `max_age`; a stale table is not read. The
check is cached for a tenth of `max_age`, clamped to between five seconds
and five minutes. An engine with no executor never routes at all, because
compiled output must not depend on state nothing verified.

**A failed check falls back rather than refusing.** A rollup is an
optimisation. Turning a broken rollup build into an outage for questions
the source tables answer perfectly well is the wrong trade, and the
operator finds out from `GET /v1/health`, which reports every declared
rollup, when it was last probed, and why one is not being used.

**Only distributive metrics can be in a rollup.** SUM, COUNT, MIN and MAX
can be recomputed from partial aggregates. A `COUNT(DISTINCT ...)`, an
`AVG` and any ratio cannot, and listing one fails the load rather than
returning a wrong number for it. Roll up the metrics a ratio is built
from instead.

**The recombining function is derived, not declared.** A stored SUM
recombines with SUM. A stored COUNT recombines with **SUM**, because
counting the rollup's rows counts groups rather than the rows underneath
them, and COUNT there returns a smaller number in the right units that
nothing would flag. This is why the file has no place to write it.

**A time dimension needs a `grain`.** A request at that grain or coarser
is served; a finer one is not, because the detail is gone. Week is its own
case: a week can be built from days, and nothing can be built from weeks
except weeks, because a week straddles a month boundary.

**A filter on a column the rollup does not carry is a fallback**, because
dropping the filter would return rows the caller excluded. A filter on a
time column is only applied when the stored grain is no coarser than the
filter's own precision.

Governance is unaffected. The plan is built against the model and only
then rewritten to read the rollup, so the fan-out check, the grain check
and the access gate all run on the real fields. A grant written on
`customers.region` keeps applying when a rollup carries a copy of that
column.

Every answer that came from one says so: the rollup name is on the
compiled response, on the audit event, and in health.

## Model review checklist

Hand this to a coding agent alongside any model files.

1. Does every dataset declare a `primary_key`? Grain is derived from it, so a
   dataset without one cannot be checked for fan-out and is the single most
   likely cause of a wrong number.
2. Is the `primary_key` actually unique in the warehouse? Nothing here can
   verify that, and a declared key that is not unique produces silent
   inflation that looks exactly like a correct answer.
3. Are foreign keys declared in relationships rather than as plain dimensions?
4. Do key names match exactly across datasets?
5. Does every dataset with metrics have a time dimension with an explicit base
   granularity?
6. Does every metric use exactly one aggregate function?
7. Are ratio metrics computed after aggregation rather than as an average of
   ratios?
8. Is there any path where a one-to-many relationship could fan out a sum? The
   planner will refuse it, so this question is really: is the refusal one you
   expect, or a modelling mistake?
9. Are there two or more join paths between the same pair of datasets? If so,
   is the preferred path declared explicitly? An ambiguous path is refused.
10. Does every metric have a description written for a non-technical reader?
    An agent picks the metric from the description, so this is a correctness
    property rather than a courtesy.
11. Is any metric defined more than once?
12. Are there parity tests comparing each metric to a known-good query?
13. If there are rollups, does each one's build job actually write the
    freshness column? A rollup whose `built_at` never moves is one the
    engine correctly stops using, and the symptom is a slow dashboard
    rather than an error.
