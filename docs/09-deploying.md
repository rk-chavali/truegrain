# Deploying it

Written because the gap between "I ran the binary" and "I deployed this" was
mostly undocumented, and the undocumented part included the encryption.

## The one thing to get right

**`truegrain serve rest` speaks plain HTTP, and `truegrain serve postgres`
speaks the PostgreSQL protocol unencrypted unless given a certificate.**

That is a deliberate design, not an omission: TLS in front of an application
is a solved problem that ingress controllers, service meshes and load
balancers all do better than a Go process would, and terminating it in the
engine would mean certificate reloading, cipher policy and renewal inside a
program whose job is compiling SQL.

It is also a trap, because a binary that starts and serves looks finished.
Callers send bearer tokens. On a plain HTTP listener reachable by anything
other than loopback, those tokens are readable by anything on the path.

So, exactly one of these:

| Where it runs | What terminates TLS |
|---|---|
| Kubernetes | An Ingress with `tls`, or a mesh with mTLS between pods |
| Cloud Run | The platform, which will not serve you plain HTTP |
| A VM behind a load balancer | The load balancer, with the engine bound to a private interface |
| A VM on its own | nginx or Caddy in front, engine bound to `127.0.0.1` |
| Your laptop | Nothing. Bind to `127.0.0.1` and it never leaves the machine |

The Helm chart refuses to render an Ingress without `ingress.tls`, and
refuses to enable the wire protocol with a token and no certificate. Those
are the two combinations that quietly put a credential on a wire.

### The wire protocol is the sharper edge

A PostgreSQL client sends its password in the startup exchange. For
`serve postgres` the token **is** the password, so an unencrypted listener
with `-token-env` set puts the credential on the wire on every single
connection.

The binary refuses that combination:

```
refusing to ask clients for a token over an unencrypted socket.
```

Pass `-tls-cert` and `-tls-key`, put it behind something that terminates
TLS and bind it to loopback, or pass `-insecure` if you have read the
sentence and accept it.

```bash
truegrain serve postgres \
  -models /models \
  -addr 0.0.0.0:5439 \
  -tls-cert /tls/tls.crt \
  -tls-key  /tls/tls.key \
  -token-env TRUEGRAIN_TOKEN
```

A client then connects with `sslmode=require`. With no certificate the
engine answers `N` to the client's `SSLRequest`, so a client set to `prefer`
silently falls back to plaintext. Set `sslmode=require` on anything that
matters, so a misconfiguration fails instead of downgrading.

## Liveness and readiness

Three endpoints, three questions. They are not interchangeable and pointing
a probe at the wrong one causes an outage rather than preventing one.

| Endpoint | Question | Touches the warehouse |
|---|---|---|
| `GET /healthz` | Should this process be restarted | No |
| `GET /readyz` | Should traffic go here right now | No |
| `GET /v1/health` | What does this deployment enforce | Reports on it |

**Do not point a probe at `/v1/health`.** It reports warehouse state, so a
warehouse outage would fail every probe: liveness would restart every
replica during somebody else's incident, and a restart cannot fix a
warehouse. Readiness would pull every replica out of the load balancer at
once, which turns a partial outage into a total one, because with the
warehouse down the engine still answers every metadata route, still compiles
and still refuses a fan-out.

Neither probe is authenticated. A kubelet carries no bearer token, and
making the probe the one route that needs credentials is how a deployment
ends up with no probes configured. They disclose nothing: liveness is a
constant and readiness says only whether a model is loaded.

## Replicas

Everything scales horizontally except one thing.

**Asynchronous jobs are held in memory per process, unless you share them.**
Without a shared store, a caller that submits to `/v1/jobs` and polls can be
routed to a replica that has never heard of that job. `/v1/health` reports
which store is configured rather than leaving it to be discovered.

```bash
export TRUEGRAIN_JOBS=postgres://engine@localhost:5432/truegrain_state
truegrain serve rest -job-store-dsn-env TRUEGRAIN_JOBS ...
```

Point it at the engine's **own** database, not the warehouse: this is the
engine's state, and the warehouse is something it otherwise only ever reads.
The table is created on connect, so there is no migration to run.

With that set, submit on one replica and poll on any other. Without it, run
any number of replicas if nothing uses `/v1/jobs` (which covers the REST
query path, MCP, the wire protocol and every metadata route), and one
replica or session affinity if something does.

One thing a shared store still cannot do: **cancelling a job only stops the
query on the replica running it.** A `context.CancelFunc` is a pointer in one
process's memory. On any other replica the job is marked cancelled, so the
caller stops waiting and the result is discarded, and the warehouse keeps
going until the query's own deadline. That is the honest boundary rather
than a bug to find later.

## Bounding the damage

Three limits worth setting, none of which is on by default, and the engine
reports each one it does not have.

```bash
-max-concurrent-queries=16   # one caller cannot saturate the warehouse
-max-bytes-billed=1000000000 # BigQuery refuses a query that would cost too much
-token-scopes=read:model     # a CI credential that cannot run a query
```

The concurrency cap is off by default because the right number is a property
of the warehouse: BigQuery absorbs hundreds, a small Postgres struggles past
a couple of dozen. A deployment knows its warehouse and should set it.

## Credentials

None of them belong in a file you commit, and the engine gives you nowhere
to put them if you tried.

- **BigQuery** authenticates with Application Default Credentials only.
  There is no key-file path in any config field. On GKE that means Workload
  Identity, on Cloud Run and Compute Engine the metadata server.
- **PostgreSQL** takes the *name* of an environment variable, `-dsn-env`,
  never the connection string. A DSN on a command line is in the process
  list and in shell history.
- **A shared token** is the same: `-token-env` names the variable.
- In `truegrain.yaml`, `${VAR}` reads from the environment, and an unset
  variable fails the load rather than becoming an empty string.

### Turning one off

A leaked credential is revoked with a file, re-read while the engine runs:

```yaml
version: 1
revoked:
  - jti: 7f3a91c4-...
    reason: pasted into a support ticket
    at: 2026-09-18T09:00:00Z
```

```bash
truegrain serve rest -revocations /etc/truegrain/revocations.yaml ...
```

`jti` turns off one credential; `subject` turns off every credential a
workload holds and therefore takes it offline. Give the same file to
`serve postgres`, because a revocation honoured on one surface and not the
other means an operator sees a credential refused by the API and does not
know a dashboard still works with it.

### Rotating one

Revocation ends a credential. Rotation replaces it, and the only rotation
that works is overlap: the new token is accepted before the old one stops
being, so no client is ever holding something the server does not know.

`-token-env` cannot do that. An environment variable is fixed for the life
of the process, so rotating one means a restart, which means a deployment,
which is why nobody rotates. `-credentials` takes a file instead, re-read
when it changes, which is exactly what a platform secret mount updates in
place:

```yaml
version: 1
tokens:
  - id: ci-2026-q3
    token: ${OLD_TOKEN}
    subject: ci@example.com
    groups: [analysts]
    not_after: 2026-10-01T00:00:00Z
  - id: ci-2026-q4
    token: ${NEW_TOKEN}
    subject: ci@example.com
    groups: [analysts]
```

```bash
truegrain serve rest -credentials /etc/truegrain/credentials.yaml ...
```

Add the new entry, roll the clients, remove the old one. `GET /v1/health`
reports how many are live so you can watch the count go back to one
without reading the secret mount. `not_after` is the backstop: it ends a
credential on a date whether or not anybody remembers to edit the file.

The file holds live credentials. Mount it from the platform secret manager
and do not commit it. It is the same category as a kubeconfig. `id` exists
so a log line can name which credential was used without naming the
credential.

A file that briefly cannot be read keeps the last good list rather than
clearing it, because a secret mount is inconsistent for a moment while the
platform swaps it and refusing every caller during that window would turn
the routine operation into the outage.

Give the same file to `serve postgres`, for the same reason the revocation
list is shared.

## Environments

One set of model files, several deployments. Dev points at a scratch
database, prod at the warehouse and a stricter policy:

```yaml
version: 1
warehouse:
  dialect: duckdb
  database: build/demo.duckdb
  max_concurrent_queries: 4
governance:
  policy: policy.yaml

environments:
  dev:
    warehouse:
      database: build/dev.duckdb
  prod:
    warehouse:
      dialect: postgres
      dsn_env: TRUEGRAIN_PROD_DSN
      max_concurrent_queries: 8
    governance:
      policy: policy.prod.yaml
```

```bash
truegrain serve rest -env prod ...     # or set TRUEGRAIN_ENV
```

An overlay changes only the fields it mentions. `dev` above keeps the
dialect, the policy and the concurrency cap from the block above it.

Two things are errors rather than conveniences.

**An unknown `-env` fails.** `-env prd` is a typo somebody will make, and
falling back to the base configuration would serve development data under
the belief that it is production, or the reverse. Both answer, and only
one is right.

**Once any environment is declared, running with none chosen fails too.**
A file with dev and prod in it plus an operator who forgot the flag is a
deployment nobody can name.

The active environment is on `GET /v1/health` and on every audited
decision, so a number can be traced to the deployment that produced it.

## Trying it

One command, no cloud account, a warehouse and both surfaces:

```bash
cd deploy/compose
docker compose up --build
```

See [deploy/compose/README.md](../deploy/compose/README.md).

## Kubernetes

```bash
helm install truegrain ./deploy/helm/truegrain -f my-values.yaml
```

See [deploy/helm/truegrain/values.yaml](../deploy/helm/truegrain/values.yaml),
which is commented as the reference. The chart's notes print what the
deployment does *not* enforce after every install, for the same reason
`health` does.

The model comes from a ConfigMap by default, which is fine for trying it and
wrong for production: git stays the source of truth, so set `model.volume`
to a git-sync sidecar or a volume your CI writes, and a metric then changes
by merging a pull request rather than by a `helm upgrade`.

## What is not here

**No Terraform.** Listed in [06-validation.md](06-validation.md) as missing
and still missing.

**The Helm chart and compose file have not been run against a real cluster
or daemon.** The chart lints, renders, and its manifests parse; the compose
file parses. Neither has been stood up. That is the honest state and it is
the first thing to check before trusting either.
