package control

import (
	"context"
	"time"
)

// Who did what to whom.
//
// The engine records decisions about queries: who asked, what was refused,
// what it cost. It has never recorded decisions about *access*, so "who
// invited this person", "who made them an admin" and "who connected this
// warehouse" lived only in whatever the container's stdout was piped to.
//
// That is backwards. An auditor's first question is the access one, and until
// this file the answer was to grep logs that may not have been kept. For a
// product whose argument is evidence, having governance decisions recorded and
// access decisions not was the sharpest inconsistency in the codebase.
//
// Three rules hold here and each one is a way this would otherwise leak.
//
// Nothing written here is a secret. Not a password, not a token, not a
// connection string, not the sealed blob from secret.go. The detail field
// carries what a person needs to understand the row and nothing more.
//
// The action is from a closed set. Free text would produce an audit nobody can
// filter, which is an audit nobody reads.
//
// A refused attempt is recorded as loudly as an allowed one. The interesting
// row in an access log is usually the one that did not work.

// Action names a thing that happened. Closed set, on purpose.
type Action string

const (
	ActionSignedIn          Action = "signed_in"
	ActionSignInRefused     Action = "sign_in_refused"
	ActionSignedOut         Action = "signed_out"
	ActionOrgClaimed        Action = "org_claimed"
	ActionInvited           Action = "invited"
	ActionInviteRevoked     Action = "invite_revoked"
	ActionInviteAccepted    Action = "invite_accepted"
	ActionRoleChanged       Action = "role_changed"
	ActionPasswordSet       Action = "password_changed"
	ActionConnectionAdded   Action = "connection_added"
	ActionConnectionRemoved Action = "connection_removed"
)

// ControlEvent is one recorded action.
//
// Named for what it is rather than `Event`, which govern already uses for a
// query decision. Two types called Event in one binary is how somebody ends up
// writing a password into the wrong one.
type ControlEvent struct {
	At         time.Time `json:"at"`
	ActorEmail string    `json:"actor_email"`
	Action     Action    `json:"action"`
	// Subject is who or what it happened to: an email, a connection name.
	Subject string `json:"subject,omitempty"`
	// Outcome is allowed or refused.
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// Outcomes.
const (
	Allowed = "allowed"
	Refused = "refused"
)

// Record writes an event.
//
// Deliberately returns nothing. A failure to record must not fail the action
// that was already taken: refusing to complete an invite because the audit
// insert failed would turn a logging outage into an outage. The error is
// swallowed here and the row is lost, which is the lesser harm and is why the
// caller also logs.
//
// Best effort in the other direction too: a caller that has already written a
// response should still call this.
func (s *Store) Record(ctx context.Context, orgID, actorID, actorEmail string, action Action, subject, outcome, detail string) {
	if outcome == "" {
		outcome = Allowed
	}
	var actor any
	if actorID != "" {
		actor = actorID
	}
	// A context that is already cancelled, because the request finished, must
	// not lose the row. This is the one place a detached context is right.
	ctx = context.WithoutCancel(ctx)
	_, _ = s.pool.Exec(ctx,
		`INSERT INTO control_events (org_id, actor_id, actor_email, action, subject, outcome, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		orgID, actor, actorEmail, string(action), subject, outcome, detail)
}

// Events reads the most recent, newest first.
func (s *Store) Events(ctx context.Context, orgID string, limit int) ([]ControlEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT at, actor_email, action, subject, outcome, detail
		 FROM control_events WHERE org_id = $1 ORDER BY at DESC, id DESC LIMIT $2`,
		orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ControlEvent{}
	for rows.Next() {
		var e ControlEvent
		if err := rows.Scan(&e.At, &e.ActorEmail, &e.Action, &e.Subject, &e.Outcome, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
