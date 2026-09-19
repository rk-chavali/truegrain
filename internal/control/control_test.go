package control_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rk-chavali/truegrain/internal/control"
)

// The control plane's store.
//
// Everything here is an access control property, so the tests are written
// as the attack rather than as the feature: can a stolen database yield a
// usable credential, can a login form be used to enumerate who works here,
// can one invite become two accounts, can the last owner lock everybody
// out.
//
// Runs against a real PostgreSQL because every guarantee in this package is
// enforced by a constraint, a transaction or a WHERE clause. A fake store
// would test the test.

// harness is a store on its own schema, plus the raw handle a test needs
// to inspect or age the rows behind it.
type harness struct {
	*control.Store
	org  string
	pool *pgxpool.Pool
}

func store(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("TRUEGRAIN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRUEGRAIN_TEST_POSTGRES_DSN is not set; see .github/workflows/ci.yml")
	}

	// Every test gets its own schema, so they cannot see each other's rows,
	// nothing has to be cleaned up in order, and they can run in parallel.
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	schema := "control_test_" + hex.EncodeToString(raw)

	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("creating a test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	scoped := dsn + "&search_path=" + schema
	if !strings.Contains(dsn, "?") {
		scoped = dsn + "?search_path=" + schema
	}

	cipher, err := control.NewCipher("a-test-key-that-is-long-enough-to-pass-the-floor")
	if err != nil {
		t.Fatal(err)
	}
	s, err := control.Open(context.Background(), scoped, cipher)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(s.Close)

	pool, err := pgxpool.New(context.Background(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	org, err := s.CreateOrg(context.Background(), "Test Org")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{Store: s, org: org.ID, pool: pool}
}

// dump reads every text-shaped column of every control table, which is what
// somebody with a database dump would have.
func (h *harness) dump(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, email, name, coalesce(password_hash,''), role, array_to_string(groups,',') FROM control_users`,
		`SELECT encode(token_hash,'hex'), user_id, user_agent, ip FROM control_sessions`,
		`SELECT encode(token_hash,'hex'), email, role FROM control_invites`,
		`SELECT id, name, dialect, secret_enc, detail::text FROM control_connections`,
	} {
		rows, err := h.pool.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(fmt.Sprint(values...))
			b.WriteString("\n")
		}
		rows.Close()
	}
	return b.String()
}

// expireInvites ages every invite past its deadline, so the expiry path can
// be tested without sleeping for a week.
func (h *harness) expireInvites(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE control_invites SET expires_at = NOW() - INTERVAL '1 day'`); err != nil {
		t.Fatal(err)
	}
}

// render is what an API response would carry for a value.
func render(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestAStolenDatabaseYieldsNoUsableCredential.
//
// The single claim that makes storing warehouse credentials defensible.
// Everything in the table is either a hash or ciphertext, and the key is
// not in the database.
func TestAStolenDatabaseYieldsNoUsableCredential(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()

	const password = "correct-horse-battery-staple"
	const secret = "postgres://real:pa55word@warehouse.internal:5432/prod"

	u, err := s.CreateUser(ctx, org, "owner@example.com", "Owner", password, control.RoleOwner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConnection(ctx, org, "prod", "postgres", secret, nil, u.ID); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateSession(ctx, u.ID, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	inviteToken, err := s.CreateInvite(ctx, org, "new@example.com", control.RoleMember, nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Read every text column of every table, the way somebody with a dump
	// would, and require that no secret appears anywhere in it.
	dump := h.dump(t)
	for _, leaked := range []struct{ name, value string }{
		{"the password", password},
		{"the warehouse credential", secret},
		{"the warehouse password", "pa55word"},
		{"the session token", token},
		{"the invite token", inviteToken},
	} {
		if strings.Contains(dump, leaked.value) {
			t.Errorf("%s is readable in the database", leaked.name)
		}
	}

	// And the credential still works for the one caller allowed to have it.
	dialect, got, err := s.ConnectionSecret(ctx, org, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if got != secret || dialect != "postgres" {
		t.Errorf("the sealed credential did not round trip")
	}
}

// TestAConnectionListNeverCarriesASecret, because the list is what every
// screen renders and a secret that reaches it reaches a browser.
func TestAConnectionListNeverCarriesASecret(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()

	u, err := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "postgres://u:hunter2@host/db"
	if _, err := s.CreateConnection(ctx, org, "prod", "postgres", secret,
		map[string]string{"host": "host", "database": "db"}, u.ID); err != nil {
		t.Fatal(err)
	}

	list, err := s.Connections(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("connections = %v", list)
	}
	if rendered := render(t, list[0]); strings.Contains(rendered, "hunter2") {
		t.Errorf("a secret reached the connection list: %s", rendered)
	}
	if list[0].Detail["host"] != "host" {
		t.Errorf("detail did not survive: %v", list[0].Detail)
	}
}

// TestACredentialCannotBeSmuggledIntoDetail, because detail is returned by
// the API and a caller that put a password there would publish it.
func TestACredentialCannotBeSmuggledIntoDetail(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)

	for _, detail := range []map[string]string{
		{"password": "hunter2"},
		{"api_key": "abc"},
		{"harmless": "postgres://u:p@h/d"}, // the same value as the secret
	} {
		_, err := s.CreateConnection(ctx, org, "c", "postgres", "postgres://u:p@h/d", detail, u.ID)
		if err == nil {
			t.Errorf("accepted a credential in detail: %v", detail)
		}
	}
}

// TestALoginFormDoesNotRevealWhoHasAnAccount.
//
// One message for every failure, and the same work done either way. The
// generic message alone does not close this: an unknown address that
// returns in microseconds while a known one takes fifty milliseconds is a
// user directory with extra steps.
func TestALoginFormDoesNotRevealWhoHasAnAccount(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, org, "real@example.com", "Real",
		"correct-horse-battery-staple", control.RoleMember, nil); err != nil {
		t.Fatal(err)
	}

	_, wrongPassword := s.Authenticate(ctx, "real@example.com", "not-the-password")
	_, noSuchUser := s.Authenticate(ctx, "ghost@example.com", "not-the-password")
	if wrongPassword == nil || noSuchUser == nil {
		t.Fatal("a bad login succeeded")
	}
	if wrongPassword.Error() != noSuchUser.Error() {
		t.Errorf("the two failures are distinguishable:\n  %v\n  %v", wrongPassword, noSuchUser)
	}

	// Timing, measured rather than asserted about. A generous bound: the
	// point is to catch the microseconds-versus-milliseconds gap that an
	// early return produces, not to police scheduler noise.
	known := timeLogin(t, s, "real@example.com")
	unknown := timeLogin(t, s, "ghost@example.com")
	ratio := float64(known) / float64(unknown)
	if ratio > 4 || ratio < 0.25 {
		t.Errorf("a known address takes %v and an unknown one %v, which is a "+
			"user directory with a stopwatch", known, unknown)
	}
}

func timeLogin(t *testing.T, s *control.Store, email string) time.Duration {
	t.Helper()
	// Three attempts, best of, so one scheduling hiccup does not fail the
	// build.
	best := time.Hour
	for range 3 {
		start := time.Now()
		_, _ = s.Authenticate(context.Background(), email, "not-the-password")
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

// TestADisabledAccountCannotLogInOrKeepASession.
//
// Disabling somebody has to end the sessions they already hold. Otherwise
// the open tab keeps working, which is the case that matters: the reason
// for disabling an account is usually that somebody has left.
func TestADisabledAccountCannotLogInOrKeepASession(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	const password = "correct-horse-battery-staple"

	u, err := s.CreateUser(ctx, org, "leaver@example.com", "Leaver", password, control.RoleMember, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateSession(ctx, u.ID, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, token); err != nil {
		t.Fatalf("the session should work before disabling: %v", err)
	}

	if err := s.UpdateUser(ctx, u.ID, control.RoleMember, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, token); err == nil {
		t.Error("a disabled account kept a working session")
	}
	if _, err := s.Authenticate(ctx, "leaver@example.com", password); err == nil {
		t.Error("a disabled account logged in")
	}
}

// TestLoggingOutEndsTheSessionOnTheServer, because a cookie the browser
// forgot is not a session that ended, and the reason somebody logs out is
// often that the laptop is not theirs.
func TestLoggingOutEndsTheSessionOnTheServer(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)

	token, err := s.CreateSession(ctx, u.ID, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, token); err == nil {
		t.Error("the session still resolves after logout")
	}
}

// TestChangingAPasswordEndsEverySession, because changing it because it may
// have leaked, and leaving the sessions opened with it alive, fixes nothing.
func TestChangingAPasswordEndsEverySession(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)

	first, _ := s.CreateSession(ctx, u.ID, "laptop", "127.0.0.1")
	second, _ := s.CreateSession(ctx, u.ID, "phone", "127.0.0.1")

	if err := s.SetPassword(ctx, u.ID, "an-entirely-different-password"); err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"laptop": first, "phone": second} {
		if _, err := s.Session(ctx, token); err == nil {
			t.Errorf("the %s session survived a password change", name)
		}
	}
	if _, err := s.Authenticate(ctx, "o@example.com", "an-entirely-different-password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

// TestOneInviteBecomesOneAccount.
//
// Single use, enforced by a conditional update inside the transaction that
// creates the account, so two people opening the same link produce one
// account and one error rather than two accounts.
func TestOneInviteBecomesOneAccount(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)

	token, err := s.CreateInvite(ctx, org, "new@example.com", control.RoleMember,
		[]string{"analysts"}, owner.ID)
	if err != nil {
		t.Fatal(err)
	}

	u, err := s.AcceptInvite(ctx, token, "New Person", "another-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "new@example.com" {
		t.Errorf("email = %q, want the invited address", u.Email)
	}
	if u.Role != control.RoleMember {
		t.Errorf("role = %q, want the invited role", u.Role)
	}
	if len(u.Groups) != 1 || u.Groups[0] != "analysts" {
		t.Errorf("groups = %v; the groups decide what governance lets them read", u.Groups)
	}

	if _, err := s.AcceptInvite(ctx, token, "Impostor", "yet-another-long-password"); err == nil {
		t.Error("the same invite created a second account")
	}
}

// TestAnInviteCannotChooseItsOwnAddress.
//
// The email comes from the invite row, never from the form. If the holder
// could name the address, one invite would be an account for anybody,
// including an address that maps to an existing policy grant.
func TestAnInviteCannotChooseItsOwnAddress(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)

	token, _ := s.CreateInvite(ctx, org, "intended@example.com", control.RoleViewer, nil, owner.ID)
	u, err := s.AcceptInvite(ctx, token, "Whoever", "a-perfectly-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "intended@example.com" {
		t.Errorf("email = %q; the address must come from the invite", u.Email)
	}
}

// TestAnInviteCannotMintAnOwner, so a compromised admin cannot create a
// peer and lock the real owner out.
func TestAnInviteCannotMintAnOwner(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)

	if _, err := s.CreateInvite(ctx, org, "new@example.com", control.RoleOwner, nil, owner.ID); err == nil {
		t.Error("an invite created an owner")
	}
}

// TestAnExpiredInviteIsRefused, because a link pasted into a chat channel
// should not be a permanent back door.
func TestAnExpiredInviteIsRefused(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)
	token, _ := s.CreateInvite(ctx, org, "new@example.com", control.RoleMember, nil, owner.ID)

	h.expireInvites(t)
	if _, err := s.AcceptInvite(ctx, token, "Late", "a-perfectly-long-password"); err == nil {
		t.Error("an expired invite was accepted")
	}
	if _, err := s.PeekInvite(ctx, token); err == nil {
		t.Error("an expired invite is still readable")
	}
}

// TestAnUnclaimedInstanceBecomesClaimed, which is the whole of first-run
// security: the window where signup is open is the seconds between the
// container starting and somebody using it.
func TestAnUnclaimedInstanceBecomesClaimed(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()

	claimed, err := s.Claimed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("a fresh instance reports itself claimed")
	}
	if _, err := s.CreateUser(ctx, org, "first@example.com", "First",
		"correct-horse-battery-staple", control.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if claimed, _ = s.Claimed(ctx); !claimed {
		t.Error("the instance is still unclaimed after the first account")
	}
}

// TestTheLastOwnerCanBeCounted, so the handler that would demote them has
// something to refuse on. An instance with no owner is one nobody can
// administer, and the only fix is editing the database by hand.
func TestTheLastOwnerCanBeCounted(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, org, "o@example.com", "O", "correct-horse-battery-staple", control.RoleOwner, nil)
	if _, err := s.CreateUser(ctx, org, "m@example.com", "M",
		"correct-horse-battery-staple", control.RoleMember, nil); err != nil {
		t.Fatal(err)
	}

	if n, _ := s.CountOwners(ctx, org); n != 1 {
		t.Errorf("owners = %d, want 1", n)
	}
	if err := s.UpdateUser(ctx, owner.ID, control.RoleOwner, nil, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountOwners(ctx, org); n != 0 {
		t.Errorf("a disabled owner still counts as one; the guard would let the "+
			"last one be turned off (got %d)", n)
	}
}

// TestTwoAccountsCannotDifferOnlyByCase, because they are one person to
// every human who looks at them and two rows to a database, and that gap
// is where an invite is accepted by the wrong account.
func TestTwoAccountsCannotDifferOnlyByCase(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, org, "Person@Example.com", "P",
		"correct-horse-battery-staple", control.RoleMember, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, org, "person@example.com", "P2",
		"correct-horse-battery-staple", control.RoleMember, nil); err == nil {
		t.Error("two accounts were created for one address")
	}
	if _, err := s.Authenticate(ctx, "PERSON@EXAMPLE.COM", "correct-horse-battery-staple"); err != nil {
		t.Errorf("the account cannot log in with a different case: %v", err)
	}
}

// TestAShortPasswordIsRefused, the one composition rule worth having.
func TestAShortPasswordIsRefused(t *testing.T) {
	h := store(t)
	s, org := h.Store, h.org
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, org, "p@example.com", "P", "short", control.RoleMember, nil); err == nil {
		t.Error("an eleven character password was accepted")
	}
}

// TestTheCipherRefusesAWeakKey and refuses to open what another key sealed.
func TestTheCipherRefusesAWeakKey(t *testing.T) {
	if _, err := control.NewCipher("too-short"); err == nil {
		t.Error("a short key was accepted")
	}

	a, err := control.NewCipher("the-first-key-which-is-long-enough-for-the-floor")
	if err != nil {
		t.Fatal(err)
	}
	b, err := control.NewCipher("a-second-key-which-is-also-long-enough-to-pass!!")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal("a warehouse password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Error("a secret opened under a different key")
	}
	if got, err := a.Open(sealed); err != nil || got != "a warehouse password" {
		t.Errorf("round trip failed: %q %v", got, err)
	}
}

// TestATamperedSecretFailsToOpen, which is what GCM buys over plain AES: a
// modified ciphertext is refused rather than decrypting to something else.
func TestATamperedSecretFailsToOpen(t *testing.T) {
	c, err := control.NewCipher("a-key-that-is-definitely-long-enough-here")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal("postgres://u:p@h/d")
	if err != nil {
		t.Fatal(err)
	}
	// Flip a character in the middle of the ciphertext.
	b := []byte(sealed)
	b[len(b)/2] ^= 'A' ^ 'B'
	if _, err := c.Open(string(b)); err == nil {
		t.Error("a tampered secret decrypted")
	}
}
