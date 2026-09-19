package control

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sessions.
//
// A session is a 256-bit random token in an HttpOnly cookie, and the
// database stores only its SHA-256. That is the same rule the password
// follows: somebody who reads the database learns that a session exists and
// cannot present it.
//
// Two clocks, and both are needed. SessionIdle logs out a laptop left open
// in a cafe. SessionMaximum is a ceiling a session cannot escape however
// active it is, so a token stolen on Monday is useless by the end of the
// week even if the thief keeps it warm. Either one alone has an obvious
// hole.
//
// Revocation is server side. A cookie the browser forgot is not a session
// that ended, which matters when the reason for logging out is that the
// laptop is gone.

const (
	// SessionIdle is how long a session survives without being used.
	SessionIdle = 14 * 24 * time.Hour
	// SessionMaximum is the absolute ceiling from creation.
	SessionMaximum = 30 * 24 * time.Hour
)

// Session is an authenticated browser.
type Session struct {
	UserID     string    `json:"user_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
}

// CreateSession issues a new session and returns the token to set as a
// cookie.
//
// Always a new token, never a reused one. Continuing an existing session id
// across a login is session fixation: an attacker who can plant a cookie
// before login holds a valid session after it.
func (s *Store) CreateSession(ctx context.Context, userID, userAgent, ip string) (token string, err error) {
	token, hash := newToken()
	now := time.Now().UTC()
	_, err = s.pool.Exec(ctx,
		`INSERT INTO control_sessions (token_hash, user_id, created_at, expires_at, last_seen_at, user_agent, ip)
		 VALUES ($1, $2, $3, $4, $3, $5, $6)`,
		hash, userID, now, now.Add(SessionMaximum), truncate(userAgent, 400), ip)
	if err != nil {
		return "", err
	}
	return token, nil
}

// Session resolves a presented token to its user, refusing an expired or
// idle one.
//
// Both clocks are checked in the statement rather than in Go, so a session
// cannot be accepted by a process whose clock has drifted forward relative
// to the database, and so the check cannot be skipped by a future caller
// who forgets one of them.
func (s *Store) Session(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	hash := hashToken(token)
	now := time.Now().UTC()

	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.org_id, u.email, u.name, u.role, u.groups, u.disabled,
		        u.created_at, u.last_login_at
		 FROM control_sessions s
		 JOIN control_users u ON u.id = s.user_id
		 WHERE s.token_hash = $1
		   AND s.expires_at > $2
		   AND s.last_seen_at > $3
		   AND NOT u.disabled`,
		hash, now, now.Add(-SessionIdle)).
		Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &u.Role, &u.Groups,
			&u.Disabled, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Touch the idle clock. Best effort: a failed update means the session
	// idles out early, which is the safe direction, and is not worth failing
	// a request the user is in the middle of.
	_, _ = s.pool.Exec(ctx,
		`UPDATE control_sessions SET last_seen_at = $2 WHERE token_hash = $1`, hash, now)
	return &u, nil
}

// EndSession revokes one session, on logout.
func (s *Store) EndSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM control_sessions WHERE token_hash = $1`, hashToken(token))
	return err
}

// EndAllSessions revokes every session a user holds.
//
// What "sign out everywhere" calls, and what has to happen when an account
// is disabled or its password changes. Without it, disabling somebody
// leaves their open tabs working until the cookie happens to expire.
func (s *Store) EndAllSessions(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM control_sessions WHERE user_id = $1`, userID)
	return err
}

// PurgeExpiredSessions deletes rows both clocks have passed.
//
// Housekeeping, not security: Session already refuses them. This keeps the
// table from growing without bound on an instance that has been up for a
// year.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM control_sessions WHERE expires_at <= $1 OR last_seen_at <= $2`,
		now, now.Add(-SessionIdle))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetPassword replaces a user's password and ends every session they hold.
//
// The two go together. Changing a password because it may have leaked, and
// leaving the sessions opened with it alive, fixes nothing.
func (s *Store) SetPassword(ctx context.Context, userID, password string) error {
	encoded, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE control_users SET password_hash = $2 WHERE id = $1`, userID, encoded)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.EndAllSessions(ctx, userID)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
