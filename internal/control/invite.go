package control

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Invites.
//
// The half oauth2-proxy cannot give you. Putting a proxy in front of the
// engine gets browser login with no code, and access then means "whoever is
// in the Google Workspace domain", managed in Google. That is the right
// answer for a company that already runs Okta and the wrong one for a team
// that wants to add one person.
//
// An invite token is a credential: whoever holds it becomes a member at the
// role it names. So it is treated as one. Random, single use, expiring, and
// stored hashed, exactly like a session.
//
// The row survives being used rather than being deleted, because "who let
// this person in" is a question an audit asks a year later and a deleted
// row cannot answer.

// InviteLifetime is how long an invite stays usable.
//
// Seven days. Long enough to survive a holiday, short enough that a link
// pasted into a chat channel is not a permanent back door.
const InviteLifetime = 7 * 24 * time.Hour

// Invite is a pending or used invitation.
type Invite struct {
	Email      string     `json:"email"`
	Role       Role       `json:"role"`
	Groups     []string   `json:"groups"`
	InvitedBy  string     `json:"invited_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
}

// Pending reports whether this invite can still be used.
func (i Invite) Pending() bool {
	return i.AcceptedAt == nil && time.Now().Before(i.ExpiresAt)
}

// CreateInvite issues one and returns the token to put in a link.
//
// The token is returned exactly once and never stored, so an admin who
// loses the link issues a new invite rather than looking the old one up.
// That is the property being bought: the database cannot hand anybody a
// working invitation.
//
// An owner cannot be invited. Ownership is the one role that has to be
// granted by an existing owner after the fact, so that a compromised admin
// cannot mint a peer and lock the real owner out.
func (s *Store) CreateInvite(ctx context.Context, orgID, email string, role Role, groups []string, invitedBy string) (token string, err error) {
	email = NormaliseEmail(email)
	if err := CheckEmail(email); err != nil {
		return "", err
	}
	switch role {
	case RoleAdmin, RoleMember, RoleViewer:
	default:
		return "", errors.New(
			"an invite may be admin, member or viewer. Ownership is granted by " +
				"an existing owner after the account exists, so that an invite " +
				"cannot create a second owner")
	}
	if groups == nil {
		groups = []string{}
	}

	// An address that already has an account gets a clear error rather than
	// an invite that will fail on acceptance.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM control_users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "", ErrEmailTaken
	}

	// Replace any outstanding invite for the same address, so re-inviting
	// somebody does not leave two live tokens.
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM control_invites WHERE org_id = $1 AND email = $2 AND accepted_at IS NULL`,
		orgID, email); err != nil {
		return "", err
	}

	token, hash := newToken()
	now := time.Now().UTC()
	_, err = s.pool.Exec(ctx,
		`INSERT INTO control_invites (token_hash, org_id, email, role, groups, invited_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hash, orgID, email, string(role), groups, invitedBy, now, now.Add(InviteLifetime))
	if err != nil {
		return "", err
	}
	return token, nil
}

// Invites lists them for the people screen.
func (s *Store) Invites(ctx context.Context, orgID string) ([]Invite, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT email, role, groups, invited_by, created_at, expires_at, accepted_at
		 FROM control_invites WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Invite{}
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.Email, &i.Role, &i.Groups, &i.InvitedBy,
			&i.CreatedAt, &i.ExpiresAt, &i.AcceptedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// PeekInvite reads an unused invite without consuming it, so the signup
// form can show who is being invited and to what.
//
// Returns only what the holder of the token already knows: the address it
// was sent to and the role. Not who sent it, and nothing about anybody
// else, because this is reachable without being logged in.
func (s *Store) PeekInvite(ctx context.Context, token string) (*Invite, error) {
	var i Invite
	err := s.pool.QueryRow(ctx,
		`SELECT email, role, groups, created_at, expires_at
		 FROM control_invites
		 WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > NOW()`,
		hashToken(token)).
		Scan(&i.Email, &i.Role, &i.Groups, &i.CreatedAt, &i.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// AcceptInvite creates the account the invite describes.
//
// One transaction, and the invite is marked used inside it with a
// conditional update. Two people opening the same link at the same moment
// produce one account and one error rather than two accounts, because the
// UPDATE that finds no unaccepted row loses the race and rolls back.
//
// The email comes from the invite, never from the form. A signup that let
// the holder choose their own address would turn one invite into an
// account for anybody.
func (s *Store) AcceptInvite(ctx context.Context, token, name, password string) (*User, error) {
	if err := CheckPasswordPolicy(password); err != nil {
		return nil, err
	}
	hash := hashToken(token)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID, email string
	var role Role
	var groups []string
	err = tx.QueryRow(ctx,
		`UPDATE control_invites SET accepted_at = NOW()
		 WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > NOW()
		 RETURNING org_id, email, role, groups`, hash).
		Scan(&orgID, &email, &role, &groups)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("that invitation has already been used, or it has expired")
	}
	if err != nil {
		return nil, err
	}

	encoded, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u := &User{
		ID: newID("usr"), OrgID: orgID, Email: email, Name: name,
		Role: role, Groups: groups, CreatedAt: time.Now().UTC(),
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO control_users (id, org_id, email, name, password_hash, role, groups, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.ID, u.OrgID, u.Email, u.Name, encoded, string(u.Role), u.Groups, u.CreatedAt); err != nil {
		return nil, ErrEmailTaken
	}
	if _, err := tx.Exec(ctx,
		`UPDATE control_invites SET accepted_by = $2 WHERE token_hash = $1`, hash, u.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return u, nil
}

// RevokeInvite withdraws an unused invitation.
func (s *Store) RevokeInvite(ctx context.Context, orgID, email string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM control_invites WHERE org_id = $1 AND email = $2 AND accepted_at IS NULL`,
		orgID, NormaliseEmail(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
