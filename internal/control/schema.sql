-- The control plane's own tables.
--
-- Its own database, never the warehouse. The warehouse holds somebody's
-- business data and is frequently read-only to this process; this holds who
-- may log in. Putting them together is how a semantic layer ends up needing
-- write access to a production warehouse.
--
-- Applied at startup and idempotent, so a restart is not a migration event
-- and a fresh volume comes up working. There is no migration tool here
-- deliberately: one table set, created if absent, is the whole of what a
-- v1 needs, and a framework would be four hundred lines to run this file.

CREATE TABLE IF NOT EXISTS control_orgs (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS control_users (
  id            TEXT PRIMARY KEY,
  org_id        TEXT NOT NULL REFERENCES control_orgs(id) ON DELETE CASCADE,
  -- Stored lowercased. Two accounts differing only in case are one account
  -- to every human who looks at them and two to a database, and that gap is
  -- where an invite gets accepted by the wrong person.
  email         TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL DEFAULT '',
  -- The encoded argon2id hash, never a password. Null for an account that
  -- signs in through an identity provider and has no local password.
  password_hash TEXT,
  role          TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
  -- Which groups the governance gate sees for this user. The whole point of
  -- the control plane is that these reach `govern.Identity`, so a policy
  -- written against `analysts` applies to a person who logged in.
  groups        TEXT[] NOT NULL DEFAULT '{}',
  disabled      BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS control_sessions (
  -- The SHA-256 of the session token, never the token. A stolen database
  -- has to be able to identify sessions without being able to present one,
  -- which is the same reason a password is hashed.
  token_hash  BYTEA PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES control_users(id) ON DELETE CASCADE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  -- Two clocks. expires_at is an absolute ceiling a long-lived session
  -- cannot escape; last_seen_at drives the idle timeout. One alone is not
  -- enough: absolute-only logs an active user out mid-task, idle-only means
  -- a stolen cookie lives forever as long as it keeps being used.
  expires_at  TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  user_agent  TEXT NOT NULL DEFAULT '',
  ip          TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS control_sessions_user ON control_sessions(user_id);

CREATE TABLE IF NOT EXISTS control_invites (
  -- Hashed for the same reason a session token is. An invite token is a
  -- credential: whoever holds it becomes a member.
  token_hash  BYTEA PRIMARY KEY,
  org_id      TEXT NOT NULL REFERENCES control_orgs(id) ON DELETE CASCADE,
  email       TEXT NOT NULL,
  role        TEXT NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
  groups      TEXT[] NOT NULL DEFAULT '{}',
  invited_by  TEXT NOT NULL REFERENCES control_users(id) ON DELETE CASCADE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at  TIMESTAMPTZ NOT NULL,
  -- Set when used. The row is kept rather than deleted so that "who invited
  -- this person" survives, which is the question an audit asks.
  accepted_at TIMESTAMPTZ,
  accepted_by TEXT REFERENCES control_users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS control_invites_org ON control_invites(org_id);

CREATE TABLE IF NOT EXISTS control_connections (
  id          TEXT PRIMARY KEY,
  org_id      TEXT NOT NULL REFERENCES control_orgs(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  dialect     TEXT NOT NULL,
  -- AES-GCM ciphertext. The key is in the environment and never in this
  -- database, so a dumped backup is not a leaked warehouse.
  secret_enc  TEXT NOT NULL,
  -- Everything about the connection that is not the secret: host, database,
  -- warehouse name. Safe to show in a list and safe to log.
  detail      JSONB NOT NULL DEFAULT '{}',
  created_by  TEXT NOT NULL REFERENCES control_users(id) ON DELETE CASCADE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  -- What happened the last time anything tried to use it. An operator
  -- should learn a credential expired from this screen rather than from a
  -- user reporting that a dashboard broke.
  last_ok_at  TIMESTAMPTZ,
  last_error  TEXT NOT NULL DEFAULT '',
  UNIQUE (org_id, name)
);

-- One organisation per instance today, and the table exists anyway.
--
-- Adding an org_id later means rewriting every query and every index in
-- this file under load. Carrying one column now costs nothing and is the
-- difference between "add a second team" being a feature and being a
-- migration.

-- Who did what to whom.
--
-- Separate from the engine's audit log, which records decisions about
-- queries and whose shape is entirely query-shaped: metrics, dimensions, a
-- SQL hash, bytes billed. None of that describes "invited bob as admin", and
-- forcing one row type to carry both would leave every consumer branching on
-- which kind it got.
--
-- The question this exists to answer is the first one any auditor asks, and
-- until this table the answer was "grep the container logs": show me every
-- access grant, every role change and every warehouse connection added, with
-- who did it.
--
-- Kept on delete of the actor, deliberately. `ON DELETE SET NULL` rather than
-- CASCADE, because removing a person must not remove the record of what they
-- did, which is exactly the record somebody removing themselves would want
-- gone.
CREATE TABLE IF NOT EXISTS control_events (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  org_id     TEXT NOT NULL REFERENCES control_orgs(id) ON DELETE CASCADE,
  at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  -- The actor, by id and by email. The email is copied rather than joined so
  -- the record still names somebody after their row is gone.
  actor_id    TEXT REFERENCES control_users(id) ON DELETE SET NULL,
  actor_email TEXT NOT NULL,
  -- What happened, from a closed set the application writes. Not free text:
  -- an audit nobody can filter is an audit nobody reads.
  action     TEXT NOT NULL,
  -- Who or what it happened to: an email, a connection name, a role.
  subject    TEXT NOT NULL DEFAULT '',
  -- Whether it took effect. A refused attempt is the more interesting row.
  outcome    TEXT NOT NULL DEFAULT 'allowed' CHECK (outcome IN ('allowed', 'refused')),
  -- Anything else worth keeping, as text. Never a secret, never a password,
  -- never a connection string: see secret.go for why those live nowhere but
  -- the sealed column.
  detail     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS control_events_org_at ON control_events (org_id, at DESC);
