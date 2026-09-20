# The whole thing, in one command

```bash
cd deploy/compose
docker compose up --build
```

Four containers: a PostgreSQL holding the demo fixture, the console on
`8088`, the REST API on `8081`, and the same model on the PostgreSQL wire
protocol on `5439`.

Then open <http://localhost:8088> and create the first account. Nothing is
seeded, so there is no default password to forget to change, and sign-up
closes as soon as that account exists.

Ports are what was free rather than what is conventional: `8080` is the
most contested port on a developer machine, so the console took `8088` and
the API took `8081`.

## What to try

**In the browser.** Sign in, pick `order_revenue`, run it. Then group it by
`order_lines.item_id` and watch it refuse: that question would answer
`2361.00`, the truth is `885.50`, and the engine names the metric that does
answer rather than inflating a total. One click swaps it in.

**On the command line**, against the API container on `8081`. It wants a
token because it binds every interface, and the token below is the one
`compose.yaml` sets: public on purpose, because nothing outside the compose
network can reach the port.

```bash
export TRUEGRAIN_TOKEN=demo-only-token-not-a-real-secret
```

A governed number:

```bash
curl -s localhost:8081/v1/query \
  -H "Authorization: Bearer $TRUEGRAIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"metrics":["order_revenue"],"dimensions":["customers.region"]}'
```

The refusal, which is the part worth seeing:

```bash
curl -s localhost:8081/v1/query \
  -H "Authorization: Bearer $TRUEGRAIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"metrics":["order_revenue"],"dimensions":["order_lines.item_id"]}'
```

That question would answer `2361.00`. The truth is `885.50`, and the engine
declines rather than inflating a total, naming the metrics that do answer it.

The same model as a database, which is how a BI tool reaches it:

```bash
psql "host=127.0.0.1 port=5439 dbname=truegrain user=you" \
  -c 'SELECT "customers.region", order_revenue FROM retail GROUP BY 1'
```

Point Metabase, Superset or DBeaver at `127.0.0.1:5439`, any database name,
any user, no password.

## What this is not

Not a production deployment, in three specific ways.

**The passwords are in the compose file.** They are fixed and public, which
is safe only because nothing outside the compose network can reach the
database and there is nothing in it but the fixture.

**The engine is unauthenticated.** Every caller is `you@example.com` and no
policy file is loaded, so `health` reports that nothing is enforced. That
report is the honest one and worth reading:

```bash
curl -s localhost:8081/v1/health \
  -H "Authorization: Bearer $TRUEGRAIN_TOKEN" | python -m json.tool
```

**Everything binds to loopback**, so a laptop on a shared network does not
expose a warehouse while somebody reads this page.

For a real deployment see `deploy/helm`, and read
[docs/09-deploying.md](../../docs/09-deploying.md) for the TLS posture, which
is the thing most easily got wrong.

## What is demo-only here

Three things, and all three are wrong anywhere else.

**The passwords are in this file.** The warehouse password and the API
token are fixed and public, which is safe only because every port binds to
loopback and the database holds nothing but the fixture.

**`TRUEGRAIN_CONTROL_KEY` is fixed.** It encrypts any warehouse credential
you enter in the browser. Generate a real one with `openssl rand -base64
32` and keep it out of source. Changing it later does not re-encrypt what
is already stored, so those connections have to be entered again.

**There is no TLS.** The console takes a password and issues a session
cookie, and both cross the network in the clear. The binary refuses to do
that on a real address without `-insecure`, which is passed here on
purpose.

**The control plane shares a PostgreSQL server with the warehouse**, in a
separate database. Production separates them: one holds who may sign in,
the other holds somebody's business data.
