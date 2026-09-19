package govern_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Token rotation.
//
// The only rotation that works is overlap: the new token is accepted before
// the old one stops being, so no client is ever holding a credential the
// server does not know. Every test here is about that window, or about the
// ways a file can be wrong in a manner that would look like an
// authentication bug instead of like configuration.

// quickReload shortens the stat throttle for one test. It is a performance
// knob, not a correctness one, and waiting it out twice per rotation is
// eighteen seconds of CI spent proving nothing.
func quickReload(t *testing.T) {
	t.Helper()
	t.Cleanup(govern.SetReloadIntervalForTest(20 * time.Millisecond))
}

func writeCredentials(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rewrite replaces the file and moves its timestamp forward.
//
// Explicitly, because a rewrite inside the filesystem's timestamp
// granularity is exactly what a scripted rotation looks like and the size
// may not change either. The loader compares both; a test that relied on
// the clock would pass or fail depending on how fast the machine is.
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

const oneToken = `
version: 1
tokens:
  - id: ci-2026-q3
    token: old-secret
    subject: ci@example.com
    groups: [analysts]
`

const bothTokens = `
version: 1
tokens:
  - id: ci-2026-q3
    token: old-secret
    subject: ci@example.com
    groups: [analysts]
  - id: ci-2026-q4
    token: new-secret
    subject: ci@example.com
    groups: [analysts]
`

const newTokenOnly = `
version: 1
tokens:
  - id: ci-2026-q4
    token: new-secret
    subject: ci@example.com
    groups: [analysts]
`

// TestARotationHasNoWindowWhereNeitherTokenWorks.
//
// The whole point. Three states, and at every one of them at least one of
// the two tokens a client could be holding is accepted.
func TestARotationHasNoWindowWhereNeitherTokenWorks(t *testing.T) {
	quickReload(t)
	path := writeCredentials(t, oneToken)
	creds, err := govern.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}

	accepts := func(token string) bool {
		_, ok := creds.Lookup(token)
		return ok
	}

	if !accepts("old-secret") || accepts("new-secret") {
		t.Fatal("the starting state is wrong")
	}

	// Both live: clients roll over here, in whatever order they restart.
	rewrite(t, path, bothTokens)
	waitForReload(t, creds)
	if !accepts("old-secret") {
		t.Error("the old token stopped working the moment the new one was added, " +
			"which is the outage rotation is supposed to avoid")
	}
	if !accepts("new-secret") {
		t.Error("the new token was added to the file and is not accepted")
	}

	// The old one is removed once nothing holds it.
	rewrite(t, path, newTokenOnly)
	waitForReload(t, creds)
	if accepts("old-secret") {
		t.Error("the old token still works after being removed from the file")
	}
	if !accepts("new-secret") {
		t.Error("the new token stopped working")
	}
}

// waitForReload drives the throttled stat forward.
//
// The loader only checks the file every few seconds so a busy server does
// not stat it per request. Rather than sleeping that out, the test polls
// until the change lands and fails if it never does, which keeps the test
// fast on a machine where the reload is immediate.
func waitForReload(t *testing.T, creds *govern.Credentials) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	start := creds.Active()
	for time.Now().Before(deadline) {
		if creds.Active() != start {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the file changed and the loader never picked it up (still %d active)", start)
}

// TestATokenStopsWorkingAtItsNotAfter.
//
// Removing the entry is how a credential ends, and an operator who plans a
// rotation wants the old token to die on a date whether or not anybody
// remembers to edit the file. Without this, "we rotated in March" leaves
// the March token valid forever.
func TestATokenStopsWorkingAtItsNotAfter(t *testing.T) {
	path := writeCredentials(t, `
version: 1
tokens:
  - id: expired
    token: yesterday
    subject: ci@example.com
    not_after: 2020-01-01T00:00:00Z
  - id: live
    token: today
    subject: ci@example.com
    not_after: 2099-01-01T00:00:00Z
`)
	creds, err := govern.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := creds.Lookup("yesterday"); ok {
		t.Error("a token past its not_after was accepted")
	}
	if _, ok := creds.Lookup("today"); !ok {
		t.Error("a token inside its not_after was refused")
	}
	if n := creds.Active(); n != 1 {
		t.Errorf("Active() = %d, want 1; an operator checks this to see a rotation finish", n)
	}
}

// TestTheIdentityIsTheOneTheFileNames, including the token id, so an audit
// entry can say which credential was used without saying what it was.
func TestTheIdentityIsTheOneTheFileNames(t *testing.T) {
	path := writeCredentials(t, `
version: 1
tokens:
  - id: dashboards
    token: s3cret
    subject: bi@example.com
    groups: [analysts, finance]
    scopes: [query]
`)
	creds, err := govern.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}

	id, ok := creds.Lookup("s3cret")
	if !ok {
		t.Fatal("the token was not accepted")
	}
	if id.Subject != "bi@example.com" {
		t.Errorf("subject = %q", id.Subject)
	}
	if strings.Join(id.Groups, ",") != "analysts,finance" {
		t.Errorf("groups = %v", id.Groups)
	}
	if strings.Join(id.Scopes, ",") != "query" {
		t.Errorf("scopes = %v", id.Scopes)
	}
	if id.TokenID != "dashboards" {
		t.Errorf("TokenID = %q; an audit entry has nothing else to name this "+
			"credential by", id.TokenID)
	}
}

// TestAFileThatCannotBeReadDoesNotLockEverybodyOut.
//
// A secret mount is briefly inconsistent while the platform swaps it. A
// server that refused every caller during that window would turn a routine
// rotation into an outage, which is the thing this whole mechanism exists
// to avoid.
func TestAFileThatCannotBeReadDoesNotLockEverybodyOut(t *testing.T) {
	quickReload(t)
	path := writeCredentials(t, oneToken)
	creds, err := govern.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// Poll well past the reload interval so the loader definitely tries and
	// fails several times.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := creds.Lookup("old-secret"); !ok {
			t.Fatal("the last good list was dropped when the file went missing")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestAMissingFileAtStartupIsAnError, because pointing at a path that is
// not there is a typo, and starting anyway would serve a listener that
// refuses every caller and reads as an authentication bug.
func TestAMissingFileAtStartupIsAnError(t *testing.T) {
	if _, err := govern.LoadCredentials(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing credentials file was accepted at startup")
	}
}

// TestAFileThatWouldBeAmbiguousIsRefused.
//
// Each of these parses as YAML and would produce a server that behaves
// unpredictably rather than one that fails. A duplicate token value is the
// worst: which identity a caller gets would depend on file order.
func TestAFileThatWouldBeAmbiguousIsRefused(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"no tokens", "version: 1\ntokens: []\n", "refuse every caller"},
		{"wrong version", "version: 2\ntokens: []\n", "unsupported version"},
		{"no id", "version: 1\ntokens:\n  - token: x\n    subject: a@b\n", "has no id"},
		{"no value", "version: 1\ntokens:\n  - id: a\n    subject: a@b\n", "has no value"},
		{"no subject", "version: 1\ntokens:\n  - id: a\n    token: x\n", "authenticates as nobody"},
		{
			"duplicate id",
			"version: 1\ntokens:\n  - id: a\n    token: x\n    subject: s\n  - id: a\n    token: y\n    subject: s\n",
			"share the id",
		},
		{
			"duplicate token",
			"version: 1\ntokens:\n  - id: a\n    token: x\n    subject: s1\n  - id: b\n    token: x\n    subject: s2\n",
			"same value",
		},
		{
			"unknown field",
			"version: 1\ntokens:\n  - id: a\n    token: x\n    subject: s\n    expires: 2026-01-01\n",
			"field expires",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := govern.LoadCredentials(writeCredentials(t, c.body))
			if err == nil {
				t.Fatalf("accepted a file that should not load:\n%s", c.body)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not say what is wrong:\n got %v\nwant it to mention %q",
					err, c.want)
			}
		})
	}
}

// TestNoErrorFromThisPackageCarriesAToken.
//
// Every error here is read by somebody pasting it into a ticket. A bearer
// token that appears in one has been disclosed, and the person pasting it
// will not know that.
func TestNoErrorFromThisPackageCarriesAToken(t *testing.T) {
	const secret = "sup3r-s3cret-value"
	for _, body := range []string{
		"version: 2\ntokens:\n  - id: a\n    token: " + secret + "\n    subject: s\n",
		"version: 1\ntokens:\n  - token: " + secret + "\n    subject: s\n",
		"version: 1\ntokens:\n  - id: a\n    token: " + secret + "\n",
		"version: 1\ntokens:\n  - id: a\n    token: " + secret +
			"\n    subject: s\n  - id: b\n    token: " + secret + "\n    subject: s2\n",
		"version: 1\ntokens:\n  - id: a\n    token: " + secret + "\n    subject: s\n    nope: 1\n",
	} {
		_, err := govern.LoadCredentials(writeCredentials(t, body))
		if err == nil {
			t.Fatalf("expected an error for:\n%s", body)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a token reached an error message: %v", err)
		}
	}
}
