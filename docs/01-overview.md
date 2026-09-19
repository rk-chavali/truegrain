# 01 Overview

## The problem

Business definitions live inside whichever BI tool bought them first. Switch
tools and the definitions are lost. Add an AI agent and it queries underneath
the tool, blind to all of it, producing a different number than the dashboard.

Warehouses have strong governance primitives. BigQuery has IAM, policy tags and
column-level security. What none of them ship is a layer that turns arbitrary
intent into governed, reproducible SQL.

## What this is

An engine that reads a semantic model, accepts a semantic request, and emits
dialect-correct SQL for the target warehouse, with column-level access resolved
before emission.

Three things make it different from the existing field:

1. **It refuses.** A question the model cannot answer correctly is declined at
   compile time, with a reason and a hint naming a question that can be
   answered. Validated against a real BigQuery warehouse: the refused query,
   run by hand, returns 2361 where the truth is 885.50.
2. **Agent-native.** Refusals carry a retry classification, so an agent knows
   whether to rewrite the request, wait, or stop. The MCP tool surface makes an
   ungoverned query inexpressible rather than merely discouraged.
3. **Ossie-native.** It implements the Apache Ossie specification rather than
   inventing another YAML dialect.

## Why Ossie

Open Semantic Interchange published v1.0 in January 2026 under Apache 2.0 and
entered the Apache Incubator as Apache Ossie in August 2026, backed by 50+
organizations including Snowflake, Salesforce and dbt Labs. It defines a
vendor-neutral model for data sets, metrics, dimensions, relationships and
contexts.

Adopting it means the model format is settled by committee rather than by us,
and an Ossie model written for another tool works here on day one.

One thing worth knowing before reading the rest: **Ossie declares no grain, no
cardinality and no additivity.** This engine derives all three from
`primary_key` and the relationships, which is what makes fan-out detection
possible at all. See `03-semantic-model.md`.

## What this is not

- **Not a BI tool.** There is a console, and it is an operator's view of a
  running engine rather than a dashboard builder. No charts to author, no
  reports to schedule, no drag-and-drop. See `04-interfaces.md`.
- **Not a modelling tool.** Strata writes the model; this serves it. Nothing
  here introspects a warehouse or edits YAML.
- **Not a transformation tool.** dbt and Dataform sit upstream and build the
  tables this reads.
- **Not an identity provider.** It borrows the warehouse's and the cloud's.
- **Not a cache.** Every query goes to the warehouse. There are no
  pre-aggregations and no materialisation.

## Differentiation, honestly stated

A generic Ossie-to-SQL compiler is table stakes and well-funded teams will ship
one. Being honest about where this stands:

**Proven.** Refusing a question that cannot be answered correctly, with a retry
classification an agent can act on. Cube and MetricFlow will return the
inflated number. This is the thing to lean on.

**Proven.** Telling you what is *not* enforced. `health` reports the gaps as
prominently as the controls, and a case where it overstated impersonation was
found and fixed rather than left.

**Partly proven.** Cross-cloud policy preservation. Policy tags are read and
enforced against the caller's own IAM, and that path is exercised against a
real taxonomy. What is not verified is fine-grained reader enforcement, which
needs an organizational GCP project. See `06-validation.md`, which says so
plainly rather than implying it works.

**Not built.** Importers. Converting LookML, dbt and Cube models into Ossie
would solve the cold start for anyone who already has a semantic layer, and
almost nobody has written Ossie models yet.

The cold start from a warehouse is answered: `truegrain init` reads BigQuery's
or PostgreSQL's own metadata and writes the model, keys included. Strata remains the path for
authoring a model rather than reading one. There is still no path in from an
existing semantic layer, which is the gap worth closing next.
