# Apache Ossie compliance

Every construct in the Ossie core spec and its status in this engine. If the
spec and this engine disagree, the spec wins and the divergence is recorded
here with a reason.

**Spec version targeted:** `0.2.0.dev0` (`core-spec/spec.yaml` in
[apache/ossie](https://github.com/apache/ossie))

The upstream file carries the header *"DRAFT version, schema may change before
0.2.0 is released"*. This engine tracks a moving target, and
`SupportedSpecVersions` in `parse.go` is the list it has actually been run
against. A model declaring anything else still loads, and `validate` says so.

## Documents

| Document | Status | Notes |
|---|---|---|
| `semantic_model` | supported | The whole engine |
| `ontology` | **unsupported** | A separate document kind. Loading one produces a refusal naming it as an ontology rather than a generic parse error. `examples/flights.yaml` upstream is one of these. |

## Semantic model

| Field | Status | Notes |
|---|---|---|
| `name` | supported | |
| `description` | supported | |
| `ai_context` | supported | Both the string form and the `{instructions, synonyms, examples}` form. Surfaced through MCP, where it is grounding text rather than documentation. |
| `datasets` | supported | |
| `relationships` | supported | |
| `metrics` | supported | |
| `custom_extensions` | parsed, ignored | No engine behaviour depends on a vendor extension. This is deliberate; see "Additivity" below. |

## Logical dataset

| Field | Status | Notes |
|---|---|---|
| `name` | supported | |
| `source` | supported | A dotted physical path or an inline query. Both are recognised and quoted differently. |
| `primary_key` | supported | **Read as the dataset's grain.** The spec has no grain field; `primary_key` is the only declaration of what one row means. |
| `unique_keys` | supported | Used with `primary_key` to decide whether a join can fan out. |
| `description` | supported | |
| `ai_context` | supported | |
| `fields` | supported | |
| `custom_extensions` | parsed, ignored | |

A dataset with neither `primary_key` nor `unique_keys` is a validation error.
Without one the engine must assume every join into it fans out, and every sum
across that join gets refused. Saying so at validate time is better than a
confusing refusal at query time.

## Field

| Field | Status | Notes |
|---|---|---|
| `name` | supported | |
| `expression` | supported | See "Expression language" below |
| `dimension.is_time` | supported | Including the spec's default: true for a temporal `datatype`, false otherwise |
| `label` | parsed, unused | |
| `description` | supported | |
| `datatype` | supported | The full portable vocabulary. Required for a time dimension on BigQuery, which has four truncation functions and no way to choose between them without it. |
| `ai_context` | supported | Synonyms are searchable through `list_metrics` |
| `custom_extensions` | parsed, ignored | |

A field with no `dimension:` block is readable inside metric expressions but is
not offered for grouping. That is the spec's distinction and the engine keeps it.

## Relationship

| Field | Status | Notes |
|---|---|---|
| `name` | supported | |
| `from` / `to` | supported | Direction is meaningful: `from` is the many side |
| `from_columns` / `to_columns` | supported | Positional, must be the same length |
| `ai_context` | supported | |
| `custom_extensions` | parsed, ignored | |

**Cardinality is derived, not declared.** The spec has no cardinality field and
defines a relationship as many-to-one or one-to-one by construction. The engine
reads it this way:

- `to_columns` match a declared key on `to` → traversal `from → to` is
  many-to-one and cannot multiply rows.
- Otherwise the relationship is effectively many-to-many, and the engine treats
  the join as fanning out.
- Traversing the same edge backwards (`to → from`) is one-to-many and fans out
  unless `from_columns` are also a declared key on `from`.

Self-joins are rejected. Ambiguity, meaning two or more distinct join paths
between the same pair of datasets, is a validation error: the engine never
guesses a join path and the spec has nowhere to declare a preferred one.

## Metric

| Field | Status | Notes |
|---|---|---|
| `name` | supported | |
| `expression` | supported | Must contain at least one aggregate |
| `description` | **required by this engine** | The spec makes it optional. An agent reads the description to decide whether the metric answers the question, so an undescribed metric gets picked wrongly or not at all. This is a deliberate divergence, and the strictest one here. |
| `datatype` | supported | |
| `ai_context` | supported | |
| `custom_extensions` | parsed, ignored | |

Metric-to-metric references are **not supported**. The spec defers namespacing
across metrics to a future document, so there is no defined semantics to
implement yet. A reference to another metric produces a named unsupported error.

## Additivity

The spec has no additivity field on a metric. This engine derives it from the
aggregate function the author already wrote, using the **Decomposability
Reference** in `core-spec/expression_language.md`:

| Category | Functions | Engine behaviour |
|---|---|---|
| Distributive | SUM, COUNT, MIN, MAX | Additive |
| Algebraic | AVG, STDDEV\*, VARIANCE, VAR\* | Recomputed at the requested grain |
| Holistic | MEDIAN, PERCENTILE\_\*, COUNT(DISTINCT) | Recomputed, never summed |
| Sketch-based | APPROX\_COUNT\_DISTINCT, APPROX\_PERCENTILE | Approximate, recomputed |

Deriving rather than requiring a declaration keeps a stock Ossie model working
unmodified. Requiring a `custom_extensions` entry would have meant models
authored specifically for this engine, which is a proprietary dialect wearing an
Ossie hat.

Separately, the planner asks a narrower question than decomposability: does the
aggregate survive having its input rows duplicated by a join? Only MIN, MAX, any
`DISTINCT` form, and `APPROX_COUNT_DISTINCT` do. Everything else is refused
across a fanning join.

**Semi-additive measures** (balances, inventory on hand) are the one concept
this derivation cannot recover, because nothing in the expression distinguishes
them from an ordinary sum. They are refused rather than guessed. Recovering them
needs either a spec change upstream or an extension here, and the extension has
not been added because it would break the property above.

## Expression language

Parsed from `core-spec/expression_language.md`. The ANSI\_SQL variant is what
the planner analyses; a dialect-specific variant is emitted verbatim when the
target dialect matches.

| Construct | Status |
|---|---|
| Column and qualified references | supported |
| Identifier normalization (regular uppercase, quoted exact) | supported |
| Arithmetic, comparison, logical operators | supported |
| `\|\|` concatenation | supported |
| `IS [NOT] NULL`, `[NOT] IN`, `[NOT] BETWEEN`, `[NOT] LIKE` | supported |
| `CASE` (both forms) | supported |
| `CAST`, including precision | supported |
| `EXTRACT(part FROM x)` | supported |
| `POSITION(substr IN str)` | supported |
| `INTERVAL 'n' UNIT` | parsed as an opaque literal, emitted verbatim |
| Core, statistical and percentile aggregates | supported |
| `DISTINCT` modifier | supported |
| `WITHIN GROUP (ORDER BY ...)` | supported |
| Date, string, math, conditional functions | emitted through, not validated against the spec's function list |
| Window functions (`OVER`) | **parsed then refused.** Cumulative and conversion metrics are v2. The construct is captured rather than dropped so the refusal can name it. |
| Subqueries | unsupported |
| Nested aggregates | rejected at validate time as invalid SQL |

Scalar functions outside the spec's tables are passed through to the warehouse
rather than rejected. This is a deliberate looseness: the spec's function list
is explicitly extensible per dialect, and rejecting an unrecognised scalar
function would break models that are valid for their target warehouse. The cost
is that a typo in a function name surfaces as a warehouse error rather than a
validate-time one.

## Not implemented from the wider spec

- `converters/` (dbt, GoodData, Polaris, Salesforce importers). Upstream is
  building these; see `docs/07-roadmap.md` v2 item 1.
- `compliance/` test suite.
- The SQL interface document referenced from the expression language spec.
