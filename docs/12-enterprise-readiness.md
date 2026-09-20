# 12 Enterprise readiness

What the comparable products put behind their top tier, what self-hosting
gives away for free, what is genuinely missing, and how the console has to be
arranged so none of it is hard to find.

The goal in one sentence: an enterprise evaluating this should not find a
reason to say no that is about the product rather than about their own
constraints.

## 1. What everyone else gates

Taken from the published tiers of Cube Cloud, dbt Cloud, Looker, Metabase,
AtScale, Lightdash and Preset. The pattern is consistent enough to be a
checklist.

| Gate | Who gates it | truegrain today |
| --- | --- | --- |
| SSO, SAML or OIDC | all of them | **missing for the console** |
| SCIM provisioning | dbt, Metabase, Okta-shaped vendors | missing |
| Granular RBAC | all of them | partial: owner, admin, member |
| Audit logs | Cube, Metabase, dbt | **have it, both planes** |
| Audit export to a SIEM | Metabase, dbt | missing |
| Row and column security | Metabase Pro, Looker, Cube | **have it**, no UI, no user attributes |
| Lineage and impact | dbt Explorer | missing |
| Model contracts and versions | dbt Enterprise | missing |
| Embedding and white label | Metabase, Looker | missing |
| Usage analytics | Metabase, Looker | data exists, no screen |
| Environment promotion as code | Metabase calls it serialization | **have it**, git plus env overlays |
| Alerts and scheduled delivery | Looker, Metabase, Lightdash | missing |
| Private networking, VPC, PrivateLink | Cube, dbt | **free, see below** |
| Data residency, multi region | Cube, dbt | **free, see below** |
| Dedicated infrastructure | Cube | **free, see below** |
| SLA and support | all of them | a commercial question, not a code one |

## 2. Four gates self-hosting deletes

Worth stating plainly because it is the strongest thing in this document and
it costs nothing to build.

**Private networking.** Cube and dbt charge enterprise money for VPC peering
and PrivateLink. A self-hosted engine is already inside the network. There is
nothing to peer.

**Data residency.** It runs where they install it.

**Dedicated infrastructure.** There is no shared tenancy to be moved off.

**No data leaves.** Every other layer in this list is a hosted service that
your query, and often your result, passes through. This one compiles SQL
inside your network and sends it to your warehouse. For a regulated buyer
that is not a feature comparison, it is the difference between a procurement
that finishes and one that does not.

That is four of the usual enterprise line items answered by architecture
rather than by a price list.

## 3. What is genuinely missing, in the order an enterprise will ask

### 3.1 SSO for the console

The first question, every time, and the one that stops a pilot becoming a
rollout. Nobody in a company of any size will create a password here.

The engine already verifies OIDC: `-oidc-issuer`, `-oidc-audience`,
`-subject-claim`, `-groups-claim`. What is missing is the console's own login
using the same mechanism, exchanging an authorization code for a session
rather than taking a password.

**Do not bundle an identity provider.** An enterprise has one. Accept Okta,
Entra ID, Google Workspace, Ping and anything else that speaks OIDC. Document
Dex for the minority who have no IdP and want one: it is a single Go binary
that fronts LDAP, SAML and GitHub, and it is the only reasonable thing to
suggest here. Keycloak works and is a JVM and a database, which is precisely
the weight this project refuses elsewhere.

`groups_claim` then maps directly onto roles, which also answers half of
SCIM: group membership arrives in the token and the role follows it.

### 3.2 Granular RBAC and per-namespace permission

Three roles is not enough for an organisation with several teams in one
deployment. Namespaces already carry owners and exports, so the model for
this exists; what is missing is binding a group to a namespace and a
capability. Read this namespace, deploy that one, administer none.

### 3.3 User attributes, which make row security real

Looker calls them access filters, Cube calls it the security context. The
caller carries `region=EMEA` and the row policy keys on it. truegrain has row
policies and no per-caller variable, so they can only express static rules
today. With attributes, one deployment serves many teams with one model,
which is the multi-tenant story every enterprise asks for.

They come from the OIDC token, so 3.1 has to land first.

### 3.4 Lineage and impact

dbt gates Explorer behind Enterprise and it is one of the most cited reasons
teams pay. Computable here from the model with no warehouse: datasets declare
fields, metrics reference fields, relationships connect datasets.

Two surfaces: a graph in the console, and `truegrain impact -field x.y` so a
pull request states what it breaks before anyone approves it.

### 3.5 Model contracts, versions and access modifiers

dbt's model governance, and the right answer for a layer that spans teams. A
namespace already exports a subset; the missing pieces are marking a metric
`private`, `protected` or `public`, versioning one so a breaking change is a
new version rather than a silent edit, and deprecating with a named
replacement and a date after which validate fails.

This is what makes a model shrinkable instead of only ever growing.

### 3.6 Audit export

The audit exists in both planes and stays in Postgres. An enterprise wants it
in Splunk, Chronicle or S3. Outbound only, a webhook or a file sink, no new
service.

### 3.7 Usage analytics

Which metrics are actually used, by whom, how often, at what cost. Every
number needed is already in `control_events` and `bytes_billed`. This is also
how anybody justifies deleting a metric, which makes 3.5 usable.

### 3.8 Alerts and scheduled delivery

Drift found by `doctor`, staleness, a threshold on a metric. Recorded today,
delivered never. Outbound SMTP and a webhook. Optional, because the reason
there is no mail sender is that configuring SMTP is the most common way a
self-hosted install stalls, and that reasoning still holds for making it
required.

### 3.9 Embedding and white label

A governed chart inside somebody else's application, refusing there too. A
signed embed token carrying the identity so governance still applies, which
depends on 3.3.

## 4. The self-hosting stack, and what not to add

Today: **one binary and one Postgres.** `console`, `engine` and `bi` are the
same image with different flags.

Everything in section 3 is a feature of that binary. None of it needs a new
service. Specifically:

| Tempting | Verdict |
| --- | --- |
| Keycloak for SSO | No. Accept the customer's IdP. Suggest Dex if they have none. |
| Redis for sessions and cache | No. Sessions are in Postgres, the cache is in process. Revisit only for multi replica. |
| Elasticsearch for catalogue search | No. Postgres full text search covers a model of any realistic size. |
| Kafka for the audit stream | No. Outbound webhook or file sink. |
| A scheduler service | No. A goroutine with a ticker, as `-doctor-every` already is. |
| Prometheus and Grafana | Optional and documented, never required. OTLP already exists. |
| MinIO for exports | No. Write to a path or a signed URL. |

The rule: **a new service in `compose.yaml` must justify itself against "can
this be a feature of the binary already shipped".** One binary and one
database is the distribution advantage over every hosted competitor, and it
is worth more than any single feature above.

## 5. The console has to stop being ambiguous

Eleven items in three groups today, and at least three pairs a newcomer
cannot choose between.

```
ANALYSE     Explore · Dashboards · Saved
UNDERSTAND  Catalog · Model · Activity · Governance
OPERATE     Deployment · Checks · Connections · People
```

**Catalog and Model** are both about metrics. The code is clear that one is a
search and the other a reference; the navigation is not, so the reader has to
click both to find out.

**Activity and Checks** both read as monitoring. One is the audit of
decisions, the other is model and warehouse health.

**Dashboards and Saved** both read as saved things.

**Governance** sits under UNDERSTAND while **Connections** sits under
OPERATE, though both are configuration an admin edits.

Proposed, which is also close to how Cube, Looker and Metabase arrange
themselves, so a switcher is not relearning anything:

```
EXPLORE     Explore · Saved · Dashboards
MODEL       Metrics · Lineage
OPERATE     Deployment · Health · Audit
ADMIN       Governance · Connections · People · Settings
```

What changed and why:

- **Catalog folds into Metrics** as search on the same screen. One place to
  answer "is there a metric for this", which was the newcomer's question all
  along.
- **Checks becomes Health**, which says what it reports.
- **Activity becomes Audit**, which says what it is and matches what every
  compliance reviewer will call it.
- **Governance, Connections and People move under Admin**, because all three
  are things an administrator configures and nobody else opens.
- **Lineage** joins Model, where somebody already asking about a metric is
  standing when they wonder what depends on it.

Nine items, four groups, no pair a newcomer has to guess between. Verbs where
the user acts, nouns where they read.

## 6. Order of work

1. **SSO for the console.** Unblocks 3.2, 3.3 and 3.9, and is the first
   enterprise question.
2. **Console information architecture**, as section 5. Cheap, and every
   feature after this lands in a structure that holds.
3. **Surface what exists**: cost, row policies, rollups.
4. **Lineage and impact.**
5. **Per-namespace RBAC and user attributes.**
6. **Model versions, access modifiers and deprecation.**
7. **Audit export, usage analytics, alerting.**
8. **Embedding.**

## 7. The line that does not move

Nothing that makes a number correct is ever behind a tier, a plan or a flag.
Refusal, grain, governance and the audit trail are the product. A layer whose
correctness depends on what you paid is not one worth trusting, and saying so
is worth more than any feature in this document.
