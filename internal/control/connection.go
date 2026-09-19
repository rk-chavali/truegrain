package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Warehouse connections, and the credentials that come with them.
//
// This is the part that relaxes a rule this project wrote for itself:
// "warehouse credentials stay in the environment." They no longer have to,
// because a control plane you cannot connect a warehouse from is a control
// plane nobody uses. What makes it defensible is in secret.go: the secret
// is AES-GCM sealed with a key that lives in the environment and never in
// this database, so the database alone is not enough.
//
// Three rules hold in this file and are worth stating because each one is
// a thing that would otherwise leak.
//
// No function here returns a decrypted secret to anything but the executor
// that is about to open a connection with it. Connection, the type an API
// hands back, has no field that could carry one.
//
// Nothing here logs. Not the secret, not the DSN, not the detail map, which
// can hold a host somebody considers sensitive.
//
// A connection is created and replaced, never patched. A partial update of
// a credential is how a deployment ends up with the old password and the
// new host, which fails in a way nobody reads as "the update did not
// apply".

// Connection is a configured warehouse, without its secret.
type Connection struct {
	ID      string `json:"id"`
	OrgID   string `json:"org_id"`
	Name    string `json:"name"`
	Dialect string `json:"dialect"`
	// Detail is everything that is not the secret: host, database,
	// warehouse, role. Safe to list and safe to log.
	Detail    map[string]string `json:"detail"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	// LastOKAt and LastError are what happened the last time anything tried
	// to use this. An operator should find out a credential expired here
	// rather than from somebody reporting a broken dashboard.
	LastOKAt  *time.Time `json:"last_ok_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// CreateConnection stores a warehouse and seals its secret.
//
// secret is whatever the dialect needs to authenticate: a DSN for Postgres,
// a private key for Snowflake. It is sealed before it reaches the database
// and is not retained anywhere else.
func (s *Store) CreateConnection(ctx context.Context, orgID, name, dialect, secret string, detail map[string]string, createdBy string) (*Connection, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("a connection needs a name")
	}
	if strings.TrimSpace(dialect) == "" {
		return nil, errors.New("a connection needs a dialect")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New(
			"a connection needs a credential. For Postgres that is a connection " +
				"string; for Snowflake a private key")
	}
	if detail == nil {
		detail = map[string]string{}
	}
	// Belt and braces: a caller that put the secret into detail by mistake
	// would publish it on every list. Cheaper to refuse than to audit every
	// caller forever.
	for k, v := range detail {
		if looksSecret(k) || v == secret {
			return nil, fmt.Errorf(
				"detail[%q] looks like a credential. Detail is returned by the "+
					"API and the secret is not, so it must not go there", k)
		}
	}

	sealed, err := s.cipher.Seal(secret)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}

	c := &Connection{
		ID: newID("con"), OrgID: orgID, Name: name, Dialect: dialect,
		Detail: detail, CreatedBy: createdBy, CreatedAt: time.Now().UTC(),
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO control_connections (id, org_id, name, dialect, secret_enc, detail, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (org_id, name) DO UPDATE
		   SET dialect = EXCLUDED.dialect,
		       secret_enc = EXCLUDED.secret_enc,
		       detail = EXCLUDED.detail,
		       last_error = ''`,
		c.ID, c.OrgID, c.Name, c.Dialect, sealed, encoded, c.CreatedBy, c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// looksSecret is a crude guard on a detail key.
func looksSecret(key string) bool {
	k := strings.ToLower(key)
	for _, bad := range []string{"password", "secret", "token", "key", "dsn", "credential"} {
		if strings.Contains(k, bad) {
			return true
		}
	}
	return false
}

// Connections lists them, without secrets.
func (s *Store) Connections(ctx context.Context, orgID string) ([]Connection, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, org_id, name, dialect, detail, created_by, created_at, last_ok_at, last_error
		 FROM control_connections WHERE org_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Connection{}
	for rows.Next() {
		var c Connection
		var detail []byte
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Name, &c.Dialect, &detail,
			&c.CreatedBy, &c.CreatedAt, &c.LastOKAt, &c.LastError); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(detail, &c.Detail)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConnectionSecret decrypts one connection's credential.
//
// Deliberately separate from Connections and deliberately awkward to reach.
// The only legitimate caller is the code about to open a warehouse
// connection; anything that wants to display a connection uses the other
// function, which cannot return a secret because the type has nowhere to
// put one.
func (s *Store) ConnectionSecret(ctx context.Context, orgID, name string) (dialect, secret string, err error) {
	var sealed string
	err = s.pool.QueryRow(ctx,
		`SELECT dialect, secret_enc FROM control_connections WHERE org_id = $1 AND name = $2`,
		orgID, name).Scan(&dialect, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	secret, err = s.cipher.Open(sealed)
	if err != nil {
		return "", "", err
	}
	return dialect, secret, nil
}

// RecordConnectionResult saves what happened the last time it was used.
//
// The error text is stored as the warehouse gave it. Warehouse drivers
// redact their own credentials from connection errors, which is the only
// reason this is safe; a driver that did not would need scrubbing here,
// and pgx's behaviour is already relied on for exactly this in
// internal/exec.
func (s *Store) RecordConnectionResult(ctx context.Context, orgID, name string, err error) error {
	if err == nil {
		_, e := s.pool.Exec(ctx,
			`UPDATE control_connections SET last_ok_at = NOW(), last_error = ''
			 WHERE org_id = $1 AND name = $2`, orgID, name)
		return e
	}
	_, e := s.pool.Exec(ctx,
		`UPDATE control_connections SET last_error = $3 WHERE org_id = $1 AND name = $2`,
		orgID, name, truncate(err.Error(), 2000))
	return e
}

// DeleteConnection removes one.
func (s *Store) DeleteConnection(ctx context.Context, orgID, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM control_connections WHERE org_id = $1 AND name = $2`, orgID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
