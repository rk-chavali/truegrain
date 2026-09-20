# 11 Feature map

What the widely used semantic layers do, what this one does, and what is
worth building. Written by going through Cube, the dbt Semantic Layer and
MetricFlow, AtScale, LookML, Lightdash and Metabase and asking of each
feature: does a team stop using a layer that lacks this.

The order below is that answer, not feature parity. Parity with Cube is not
reachable and is not the point.

## The decision that gates everything below

The licence is Apache 2.0, which means every feature here is free to
everybody forever. A tier that costs money needs one of three things, and
they are not interchangeable:

- **Open core.** Engine stays Apache 2.0, the paid features live under a
  separate licence in a separate directory or repository. GitLab, Cube and
  Airbyte do this. It has to be decided before the code is written, because
  retrofitting a licence boundary through a codebase is miserable.
- **Everything free, sell operation.** Support, hosting, upgrades. Acryl
  does this with DataHub. No code boundary needed.
- **Tiers as configuration.** "Enterprise" features exist and anyone can
  turn them on. Honest, costs nothing, earns nothing.

Nothing in this document assumes an answer. Where a feature is marked
**enterprise-shaped** it means large organisations need it and individuals
do not, which is a statement about who wants it rather than about who pays.

## 1. Modelling

| | truegrain | Notes |
| --- | --- | --- |
| Metrics, dimensions, joins | yes | Apache Ossie, in git |
| Grain declared and enforced | **yes, uniquely** | Nobody else refuses on it |
| Multi-namespace, cross-namespace joins | yes | Aggregated separately, joined on shared dimensions |
| **Derived metrics** | **no** | A metric defined from other metrics |
| **Time intelligence** | **no** | Period over period, cumulative, rolling |
| Semi-additive measures | partial | Additivity is derived, not declared |
| Metric deprecation and lifecycle | no | A model can only grow today |
| Visual editor | deliberately not | Strata owns authoring |

**Derived metrics** are the larger gap of the two. Today
`average_order_value` is written as `SUM(...) / COUNT(DISTINCT ...)`,
duplicating the definitions of two metrics that already exist. When one of
them changes, the derived one silently does not. Every comparable tool lets
a metric reference another metric, and it is the most common request an
adopter has after their first week.

**Time intelligence** is the one every evaluator tests in the first hour.
"Revenue this month versus last", "trailing twelve weeks", "running total
year to date". Grain bucketing exists; none of these do. A time spine table
and three metric types cover most of it.

## 2. Performance and cost

| | truegrain | Notes |
| --- | --- | --- |
| Result caching | yes | `-cache`, reported in health |
| Pre-aggregations / rollups | yes | With a freshness check that cannot be disabled |
| Query cost captured | yes | `bytes_billed` on every decision |
| **Cost visible anywhere** | **no** | Zero console files mention it |
| Cost cap per query | yes | `max_bytes_billed` |
| Scheduled rollup refresh | no | Rollups are built outside the engine |
| Aggregate recommendation | no | AtScale generates these from query patterns |

Cost is the cheapest win in this document. The number is already recorded on
every query and shown nowhere. The first question a data team asks about a
new layer is whether it will surprise them on the invoice, and the answer is
sitting in `control_events` unread.

## 3. Governance

| | truegrain | Notes |
| --- | --- | --- |
| Column-level policy | yes | Resolved before the query is sent |
| Row-level policy | yes in engine, **no UI** | `WithRowPolicies` |
| BigQuery policy tags | yes | Against a real taxonomy |
| Explain my access | yes | `policy/explain`, caller only |
| Audit of every decision | yes | Including refusals and denials |
| Control-plane audit | yes | Who invited, promoted, connected |
| **User attributes** | **no** | Looker's access filters, Cube's security context |
| SSO | no | Password only |
| SCIM provisioning | no | **enterprise-shaped** |

**User attributes** are how every other tool does multi-tenant row security:
the caller carries `region=EMEA`, the policy filters on it. truegrain has row
policies but no per-caller variable to key them on, which limits them to
static rules. This is the unlock for one deployment serving many teams.

## 4. Consumption

| | truegrain | Notes |
| --- | --- | --- |
| REST + JSON | yes | |
| MCP for agents | **yes, uniquely good** | Refusals reach the agent as refusals |
| PostgreSQL wire for BI | yes | TLS and token, one identity |
| Go, Python, TypeScript clients | yes | Zero dependencies each |
| Console with explore and charts | yes | |
| Saved queries | yes | |
| **Embeddable charts** | **no** | A governed chart in somebody else's app |
| **Scheduled delivery** | **no** | "Email me this every Monday" |
| **Alerts on a threshold** | **no** | |
| Excel and Power BI native | no | Needs MDX or DAX, large |
| GraphQL | yes | One query field |

## 5. Operations

| | truegrain | Notes |
| --- | --- | --- |
| GitOps, engine follows a repo | **yes, uniquely clean** | Pull based, no inbound rule |
| CI checks needing no warehouse | **yes, uniquely** | validate, test, diff |
| Warehouse drift detection | yes | doctor, and refresh to fix it |
| Diff on compiled SQL | **yes, uniquely** | Reports which numbers moved |
| Deploy confirmation | yes | health reports the serving commit |
| **Lineage and impact analysis** | **no** | The biggest evaluation gap |
| **Source freshness** | **no** | Rollups check it; source tables do not |
| Alerting on drift | no | Recorded, never delivered |
| Email / SMTP | deliberately not | Invite links are pasted today |

**Lineage** is the gap that loses evaluations. "If I drop this column, what
breaks" is the question every data engineer asks, and the answer is fully
computable from the model with no warehouse and no new service: datasets
declare fields, metrics reference fields, relationships connect datasets. It
is a graph derivation over YAML already parsed. That it does not exist is an
accident of ordering, not a hard problem.

## Build order

Grouped so each group ships something coherent rather than a list of
half-features.

**Group A, what already exists but cannot be seen.** No new capability, no
new service, largest ratio of value to risk.

1. Cost: spend per metric, per caller, per day, and the most expensive
   queries. The data is in `control_events`.
2. Row policies in the Governance screen.
3. Rollups in the console: which questions are covered, whether one was
   used, whether it is fresh.

**Group B, the two gaps every evaluator finds.**

4. Lineage and impact: a graph page, plus `truegrain impact -field x.y` so a
   pull request can say what it breaks.
5. Derived metrics: a metric defined from other metrics, resolved at compile
   time so the dependency cannot go stale.

**Group C, the first hour of an evaluation.**

6. Time intelligence: a time spine, period over period, cumulative, rolling
   windows.
7. Source freshness: `last_modified_time` is free metadata on BigQuery. Show
   it beside every answer and refuse against a declared SLA.

**Group D, making a model shrinkable.**

8. Metric deprecation with a named replacement. Warn on use, hide from
   agents, fail validate past a date.

**Group E, platform.**

9. User attributes, which make row policies multi-tenant.
10. SSO for the console. **Enterprise-shaped.**
11. Alert delivery for drift, staleness and thresholds. Optional SMTP and a
    webhook, both outbound, neither a new service.
12. Embeddable charts.

## What stays refused

**No new service in compose.** Every item above is a feature of the binary
already shipped. One binary and one database is the distribution advantage
over Cube and AtScale, and it is worth more than any single feature here.
Elasticsearch for search, Kafka for events, Redis for queues and a separate
scheduler are all refused on that basis.

**No visual model editor.** Strata owns authoring.

**No SQL proxy.** The wire protocol answers what a client needs to connect
and declines the rest, because being `pg_catalog` for everything means being
PostgreSQL.

**No access oracle.** `policy/explain` answers for the caller and nobody
else. Reporting what another identity can see publishes the policy.

## Where the tiers would fall, if there are tiers

Recorded as an observation about who needs what, not as a pricing plan.

**Individual and small team.** Everything in Groups A through D. A person
evaluating this needs lineage, cost, time intelligence and derived metrics
more than an enterprise does, because they have no platform team to work
around the gaps.

**Enterprise-shaped.** SSO, SCIM, user attributes for multi-tenancy, audit
export to a SIEM, and signed deployment attestation. Every one of these is
something a large organisation must have and a single user does not want.

The dividing line that stays honest: **nothing that makes a number correct
is ever behind a tier.** Refusal, grain, governance and the audit trail are
the product. A layer whose correctness depends on the plan is not one worth
trusting.
