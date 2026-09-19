# Security policy

## Reporting a vulnerability

Report privately through [GitHub Security Advisories](https://github.com/rk-chavali/truegrain/security/advisories/new).
Do not open a public issue for a vulnerability.

You will get an acknowledgement within **3 working days** and an assessment
within **10 working days**. If a fix is warranted we will agree a disclosure
date with you, and we will credit you unless you ask us not to.

## What counts as a vulnerability here

This project's purpose is to make an ungoverned query inexpressible and to prove
that an unauthorized plan never becomes SQL. Anything that undermines either is
in scope, and these are the shapes it is most likely to take:

- **A path that emits SQL without the governance gate running.** Every query
  must route through `engine.Compile`, which checks every part of a plan before
  any of it is emitted.
- **A refusal that leaks what it is refusing.** Denials name the semantic object
  the caller asked for, never the physical column, the table or the governing
  tag.
- **Governance resolved against the wrong identity for a column.** A dataset
  imported into another namespace is governed where it was defined, not where it
  is used. Anything that detaches a column from the grant written against it is
  a vulnerability, not a bug.
- **A filter value reaching the SQL text.** Values are validated at the edge and
  bound as parameters.
- **A refusal that becomes an empty result.** A silent empty answer is a
  correctness failure with security consequences: it trains a caller to report a
  wrong number confidently.
- **Audit gaps.** A decision that is made but not recorded, or recorded without
  the identity, namespace or workspace digest.
- **Fail-open behaviour.** A policy source that cannot be reached must deny.

Also in scope: credential handling, the authentication path, and anything that
widens access beyond what a policy grants.

## Known limitations, which are not vulnerabilities

These are documented rather than fixed, and reporting them will get this reply.

- **A file-backed policy governs this engine only.** It places no control on the
  warehouse. Anyone holding direct warehouse credentials is unaffected by it.
  `GET /v1/health` states this for any running deployment.
- **DuckDB enforces nothing.** It has neither column-level nor row-level
  security, so the engine's gate is the only control in that configuration.
- **Embedded use is advisory.** Linked as a library, a caller can bypass the
  engine by issuing SQL directly. Governance requires server mode.
- **Row-level security is the warehouse's job** where the warehouse has it. The
  engine does not replicate row filters it cannot see, and says so.
- **The default REST authenticator is a static token map.** It exists for local
  development. Serving it on a non-loopback address without `-token-env` is
  refused at startup.

## Supported versions

Pre-1.0. Security fixes land on `main` and in the next release. There are no
backports yet.
