package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The control plane's store.
//
// PostgreSQL through pgx, which this module already depends on for the
// Postgres executor and the shared job store, so this adds no dependency.
// Every statement here is parameterised; nothing in this file builds SQL
// from a value.
//
// Its own database, never the warehouse. See schema.sql.

//go:embed schema.sql
var schemaSQL string

// Store is the control plane's database.
type Store struct {
	pool   *pgxpool.Pool
	cipher *Cipher
}

// Role is what a member may do.
//
// Four, ordered. Anything comparing roles goes through Role.AtLeast rather
// than comparing strings, because the day somebody writes `role == "admin"`
// is the day an owner stops being able to do an admin's job.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleMember: 2, RoleAdmin: 3, RoleOwner: 4}

// AtLeast reports whether this role carries at least the authority of
// another.
func (r Role) AtLeast(other Role) bool { return roleRank[r] >= roleRank[other] && roleRank[r] > 0 }

// Valid reports whether this is a role at all, for validating input.
func (r Role) Valid() bool { return roleRank[r] > 0 }

// User is an account.
//
// There is no password field and never will be. The hash lives in the
// database and is read only by the one function that verifies it, so a
// struct that reaches an API response cannot carry one by accident.
type User struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        Role       `json:"role"`
	Groups      []string   `json:"groups"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// Org is the tenant. One per instance today; see schema.sql.
type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Open connects and applies the schema.
func Open(ctx context.Context, dsn string, cipher *Cipher) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New(
			"the control plane needs its own PostgreSQL database; name the " +
				"environment variable holding its connection string")
	}
	if cipher == nil {
		return nil, errors.New("the control plane needs a cipher; see " + KeyEnv)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		// pgx redacts the password from its own errors, so this is safe to
		// wrap. Nothing here should ever add the DSN back by hand.
		return nil, fmt.Errorf("connecting to the control plane database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to the control plane database: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("applying the control plane schema: %w", err)
	}
	return &Store{pool: pool, cipher: cipher}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Cipher is the store's secret box, for handlers that seal a value before
// handing it back.
func (s *Store) Cipher() *Cipher { return s.cipher }

// Claimed reports whether anybody has signed up yet.
//
// The whole of first-run security rests on this. An unclaimed instance lets
// the first caller create an owner; a claimed one does not, so the window
// in which an open signup form exists is the seconds between the container
// starting and the operator using it.
func (s *Store) Claimed(ctx context.Context) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM control_users`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ErrEmailTaken is returned when an address already has an account.
var ErrEmailTaken = errors.New("that email address already has an account")

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("not found")

// CreateOrg makes the tenant. Called once, by Claim.
func (s *Store) CreateOrg(ctx context.Context, name string) (*Org, error) {
	org := &Org{ID: newID("org"), Name: name, CreatedAt: time.Now().UTC()}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO control_orgs (id, name, created_at) VALUES ($1, $2, $3)`,
		org.ID, org.Name, org.CreatedAt)
	if err != nil {
		return nil, err
	}
	return org, nil
}

// Org returns the single organisation, or ErrNotFound before first run.
func (s *Store) Org(ctx context.Context) (*Org, error) {
	var o Org
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, created_at FROM control_orgs ORDER BY created_at LIMIT 1`).
		Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CreateUser adds an account.
//
// The password is hashed here rather than by the caller, so there is one
// place that decides how, and no path where a handler could store a
// plaintext by forgetting a step.
func (s *Store) CreateUser(ctx context.Context, orgID, email, name, password string, role Role, groups []string) (*User, error) {
	email = NormaliseEmail(email)
	if err := CheckEmail(email); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, fmt.Errorf("%q is not a role", role)
	}
	if groups == nil {
		groups = []string{}
	}

	var hash *string
	if password != "" {
		encoded, err := HashPassword(password)
		if err != nil {
			return nil, err
		}
		hash = &encoded
	}

	u := &User{
		ID: newID("usr"), OrgID: orgID, Email: email, Name: name,
		Role: role, Groups: groups, CreatedAt: time.Now().UTC(),
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO control_users (id, org_id, email, name, password_hash, role, groups, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.ID, u.OrgID, u.Email, u.Name, hash, string(u.Role), u.Groups, u.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "control_users_email_key") {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	return u, nil
}

// Authenticate verifies an email and password and returns the account.
//
// One error for every failure: an unknown address, a wrong password, a
// disabled account and an account with no local password are
// indistinguishable to the caller. Telling them apart is what turns a login
// form into a way to find out who works here.
//
// The timing is equalised too. A generic message with a fast "no such user"
// path still leaks the answer to anyone with a stopwatch.
func (s *Store) Authenticate(ctx context.Context, email, password string) (*User, error) {
	email = NormaliseEmail(email)

	var u User
	var hash *string
	var disabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT id, org_id, email, name, password_hash, role, groups, disabled, created_at
		 FROM control_users WHERE email = $1`, email).
		Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &hash, &u.Role, &u.Groups, &disabled, &u.CreatedAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		EqualiseTiming(password)
		return nil, ErrBadCredentials
	case err != nil:
		return nil, err
	case hash == nil, disabled:
		// An account that signs in through an identity provider, or one that
		// has been turned off. Same cost, same message.
		EqualiseTiming(password)
		return nil, ErrBadCredentials
	case !VerifyPassword(*hash, password):
		return nil, ErrBadCredentials
	}

	now := time.Now().UTC()
	u.LastLoginAt = &now
	_, _ = s.pool.Exec(ctx, `UPDATE control_users SET last_login_at = $2 WHERE id = $1`, u.ID, now)
	return &u, nil
}

// ErrBadCredentials is the only thing a failed login ever reports.
var ErrBadCredentials = errors.New("that email and password do not match an account")

// User returns one account by id.
func (s *Store) User(ctx context.Context, id string) (*User, error) {
	var u User
	var disabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT id, org_id, email, name, role, groups, disabled, created_at, last_login_at
		 FROM control_users WHERE id = $1`, id).
		Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &u.Role, &u.Groups, &disabled, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled
	return &u, nil
}

// Users lists the directory.
func (s *Store) Users(ctx context.Context, orgID string) ([]User, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, org_id, email, name, role, groups, disabled, created_at, last_login_at
		 FROM control_users WHERE org_id = $1 ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.OrgID, &u.Email, &u.Name, &u.Role,
			&u.Groups, &u.Disabled, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser changes a member's role, groups or disabled flag.
func (s *Store) UpdateUser(ctx context.Context, id string, role Role, groups []string, disabled bool) error {
	if !role.Valid() {
		return fmt.Errorf("%q is not a role", role)
	}
	if groups == nil {
		groups = []string{}
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE control_users SET role = $2, groups = $3, disabled = $4 WHERE id = $1`,
		id, string(role), groups, disabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountOwners reports how many owners the org has.
//
// Used to refuse the change that locks everybody out: demoting or disabling
// the last owner leaves an instance nobody can administer, and the only fix
// is editing the database by hand.
func (s *Store) CountOwners(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM control_users
		 WHERE org_id = $1 AND role = 'owner' AND NOT disabled`, orgID).Scan(&n)
	return n, err
}

// NormaliseEmail lowercases and trims, so one human is one account.
func NormaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CheckEmail is a deliberately shallow check.
//
// It looks for one `@` with something on each side and no spaces, and
// nothing more. A regular expression that tries to implement RFC 5322
// rejects addresses that work, and the only real proof that an address is
// deliverable is delivering to it.
func CheckEmail(email string) error {
	if len(email) > 320 {
		return errors.New("that email address is too long")
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" ||
		strings.ContainsAny(email, " \t\r\n") || strings.Contains(domain, "@") ||
		!strings.Contains(domain, ".") {
		return errors.New("that does not look like an email address")
	}
	return nil
}

// newID returns a prefixed random identifier.
//
// Random rather than sequential, because a sequential id in a URL tells
// anybody who sees one how many exist and lets them ask for the next.
func newID(prefix string) string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing means the process cannot do anything safely.
		panic("control: no entropy available: " + err.Error())
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw)
}

// newToken returns a 256-bit secret and its storage hash.
//
// The caller shows the token once and stores only the hash, which is the
// same rule a password follows and for the same reason: a stolen database
// must not yield a usable credential.
func newToken() (token string, hash []byte) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("control: no entropy available: " + err.Error())
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:]
}

// hashToken is how a presented token is looked up.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
