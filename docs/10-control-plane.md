# 10 Control plane

`truegrain serve console` is accounts, sessions, people, warehouse
connections and the UI, in the same binary as the engine and on one port.

```
/         the console, embedded in the binary
/api/     accounts, sessions, people, connections
/v1/      the engine's REST API, unchanged
```

Self-hosted, never SaaS. Download it, run it, own the database. The
reference points are DataHub and OpenMetadata rather than Cube Cloud or dbt
Cloud, and the reason is the promise this project already makes: the
warehouse never leaves your account.

## Why it is one process

DataHub splits into a frontend that runs the browser login and a metadata
service that only verifies a token. Every split like that carries the same
warning, which is that the backend must not be reachable except through the
thing that authenticates. Put the two in one process and there is no second
address to forget to firewall.

The engine's half already existed and is unchanged.
`internal/serve/rest/auth.go` has verified OIDC tokens with real signature,
issuer and audience checking since v0.2.0, and the governance gate has
always enforced column and row policy against whatever identity it was
handed. What was missing was the half above it: a login, a session, a user
table, an invitation.

So the control plane does not add an authorisation system. It adds a way for
the existing one to know who a person is:

```go
// A session cookie becomes the identity the gate already enforces against.
func identityOf(u *control.User) govern.Identity {
    return govern.Identity{Subject: u.Email, Groups: u.Groups, TokenID: u.ID}
}
```

A policy written against `analysts` now applies to a human, and the audit
log records their address instead of a shared token's subject.

## Setting it up

The first person to open an unclaimed instance creates the owner account,
and sign-up closes the moment it exists. There is no default password and
no bootstrap secret; the window in which the form is open is the seconds
between the container starting and somebody using it. Metabase and Grafana
work the same way, and it is the only model that needs nothing handed out
in advance.

After that, people join by invitation. An invitation produces a link, shown
once and never stored, which an admin sends however they already talk to
that person. There is deliberately no mail sender: configuring SMTP is the
single most common reason a self-hosted install stalls, and a link works on
a network with no outbound mail at all.

An invitation is a credential, so it is treated as one: random, single use,
expiring after seven days, and stored hashed. It cannot create an owner.
Ownership is granted by an existing owner after the account exists, so a
compromised admin cannot mint a peer and lock the real owner out.

## Logging in with Google

Two ways, and the cheap one needs no code.

**oauth2-proxy in front.** The engine already verifies OIDC tokens, so a
proxy that forwards the ID token as `Authorization: Bearer` is a complete
browser login:

```bash
oauth2-proxy --provider=google \
  --client-id=... --client-secret=... \
  --email-domain=yourcompany.com \
  --pass-authorization-header=true \
  --upstream=http://localhost:8080
```

This gives you sign-in and gives you nothing else: no invitations, no user
list, no roles screen. Access becomes whoever is in your Google Workspace
domain, managed in Google. For a company that already runs Okta that is
usually better than a second directory. It only holds if the engine is
unreachable except through the proxy.

**The control plane.** Local accounts today, with the same OIDC verifier
available for the token path. Use this when you want invitations and a
directory you can see.

## Credentials

This stores warehouse credentials, which relaxes a rule this project wrote
for itself: *warehouse credentials stay in the environment; a control plane
storing them in its own database is how this becomes a product that leaks
somebody else's warehouse.*

The rule was right about the risk and wrong about the trade. A control plane
you cannot connect a warehouse from is one nobody uses, and Metabase,
Superset and Airbyte all made the same call.

What makes it defensible is that the database alone is not enough:

- AES-256-GCM, with the key in `TRUEGRAIN_CONTROL_KEY` and never in the
  database. A dumped backup or a stolen replica is ciphertext.
- The process refuses to start without a key. There is no "encryption
  disabled" mode, because a default key is not encryption.
- No endpoint returns a secret. The type an API hands back has no field
  that could carry one, and editing a connection means typing it again.
- Nothing logs one.

A test reads every text column of every control table and requires that no
password, no warehouse credential, no session token and no invite token
appears anywhere in it.

**Rotating the key does not re-encrypt what is stored.** Connections saved
under the old key have to be entered again, and the error says so.

## What is not closed

**An admin who adds a connection makes this process connect to a host they
typed**, including one inside the network it runs in. That is the feature
and it cannot be removed without removing the feature. It takes an admin,
and every attempt is logged with who made it.

**Two people can claim an unclaimed instance at the same instant.** The
check and the insert are not one transaction. The loser fails on the unique
email constraint if they used the same address, and otherwise a second owner
exists, which the log records loudly.

**Control-plane actions are now recorded**, in `control_events`: who invited
whom, who changed a role, who connected a warehouse, and the attempts that
were refused. Read at `GET /api/events`, admin only. Deliberately a separate
trail from the engine's `/v1/audit`, which records decisions about queries and
whose shape is entirely query-shaped. Two trails because they answer different
questions and share nothing but a timestamp.

**A failed sign-in is not in that table.** It has no organisation to attribute
it to, and inserting a row keyed on a guessed one would let an unauthenticated
caller write to the audit table by typing an email address. The rate limiter
answers a password list; the request log keeps the attempt.

**The rate limiter is per process and in memory.** Two replicas mean twice
the attempts. It is protecting a login form on a self-hosted instance
against a password list, which it does.

## The database

Its own, never the warehouse. One holds who may sign in; the other holds
somebody's business data and is frequently read-only to this process.
Putting them together is how a semantic layer ends up needing write access
to production.

The schema is applied at startup and is idempotent, so a restart is not a
migration event and a fresh volume comes up working.

## Security, in one table

| Concern | What is done |
|---|---|
| Password storage | argon2id, 64 MiB, per-user salt, parameters stored with the hash so the cost can be raised later |
| Password rules | Length only, twelve characters. Composition rules push people to `Passw0rd!` |
| Credential stuffing | Per-account and per-address rate limits, one generic failure message, and a dummy hash on unknown addresses so timing does not reveal who has an account |
| Session theft | 256-bit tokens stored hashed, `HttpOnly`, `SameSite=Lax`, `__Host-` prefix over TLS, idle and absolute expiry, revoked server side |
| Session fixation | A new token on every login |
| CSRF | A custom header a cross-site form cannot add, plus `SameSite=Lax` |
| XSS | A strict CSP with `script-src 'self'`, and no `dangerouslySetInnerHTML` on anything a user supplied |
| Locking everybody out | The last owner cannot be demoted or disabled |
| Somebody leaving | Disabling an account ends every session it holds |
| A leaked password | Changing it ends every session, on every device |

## Running it

```bash
export TRUEGRAIN_CONTROL_KEY="$(openssl rand -base64 32)"
export TRUEGRAIN_CONTROL_DSN="postgres://user:pass@host:5432/truegrain_control"
export TRUEGRAIN_DSN="postgres://user:pass@warehouse:5432/analytics"

truegrain serve console \
  -models ./models \
  -dialect postgres -dsn-env TRUEGRAIN_DSN \
  -tls-cert cert.pem -tls-key key.pem \
  -addr 0.0.0.0:8080
```

TLS is not optional on a real address: people type passwords into this and
the session cookie is a credential. The binary refuses a non-loopback
address without either a certificate or `-insecure`, and behind a proxy that
terminates TLS you add `-behind-proxy` so `X-Forwarded-Proto` is believed.

The whole thing on a laptop, with a warehouse and a model:

```bash
cd deploy/compose && docker compose up
```

Then open <http://localhost:8088>.

## Backing it up

Two things, and they are only useful together.

The database holds the accounts and the warehouse credentials. The credentials
are ciphertext. The key that decrypts them is in `TRUEGRAIN_CONTROL_KEY`, and
it is deliberately not in the database, because a dumped backup that decrypts
itself is not a backup, it is a copy of your warehouse credentials.

**So: restore the database without the key and every warehouse connection is
unrecoverable.** Not corrupted, not degraded. The rows are there, they are
ciphertext, and nothing will ever read them again. Every connection has to be
created again by somebody who still has the original secrets.

That is the intended behaviour of the encryption and the thing most likely to
ruin a Tuesday. Back up both, together, and to different places:

```bash
# The database. Any ordinary PostgreSQL backup; nothing here is special.
pg_dump "$TRUEGRAIN_CONTROL_DSN" --format=custom --file=control-$(date +%F).dump

# The key. Into a secret manager, a password manager, or an envelope in a
# safe. Not next to the dump, because the point of the key being separate is
# that one stolen artefact is not enough.
echo "$TRUEGRAIN_CONTROL_KEY"
```

Restoring:

```bash
createdb truegrain_control
pg_restore --dbname=truegrain_control control-2026-09-19.dump
# Then start the engine with the SAME TRUEGRAIN_CONTROL_KEY it had before.
```

**Test the restore before you need it.** The failure this prevents is not
subtle and it is not recoverable: a team restores a database, starts the
engine with a freshly generated key because nobody wrote the old one down, and
discovers that every warehouse connection is gone at the moment they are
already having a bad day. A restore rehearsal that ends with one working
query is worth an hour.

Three smaller notes.

**Rotating the key does not re-encrypt what is stored.** Connections saved
under the old key have to be entered again, and the error says so. So a
rotation is the same event as a restore without the key, chosen deliberately.

**The model is not in here and does not need backing up.** It lives in git,
which is already a backup with better properties than this one. An engine
following a repository rebuilds its whole model from a clone.

**The audit trail is in here.** `control_events` answers who was invited, who
was made an admin, and who connected which warehouse. That is the table an
access review reads and the one most likely to be wanted long after the people
in it have left, so a retention policy that drops it is a decision to make on
purpose rather than by default.
