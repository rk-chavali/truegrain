package rest_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// Turning off a credential before it expires.
//
// The property that decides whether this is useful at all is that it takes
// effect without a restart. A deny list an operator has to restart
// production to honour is a deny list that gets honoured late, which during
// the incident it exists for is the same as not having one.

func writeList(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "revocations.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixedAuth stands in for a verified credential, so these tests are about
// the deny list rather than about JWT verification.
type fixedAuth struct{ id govern.Identity }

func (f fixedAuth) Authenticate(*http.Request) (govern.Identity, error) { return f.id, nil }
func (f fixedAuth) Name() string                                        { return "fixed (test)" }

func revoking(t *testing.T, path string, id govern.Identity) rest.Revoking {
	t.Helper()
	list, err := govern.LoadRevocations(path)
	if err != nil {
		t.Fatal(err)
	}
	return rest.Revoking{Inner: fixedAuth{id: id}, List: list}
}

func TestARevokedSubjectIsRefused(t *testing.T) {
	path := writeList(t, `
version: 1
revoked:
  - subject: sa-leaked@acme.iam.gserviceaccount.com
    reason: key found in a public repository
`)
	auth := revoking(t, path, govern.Identity{Subject: "sa-leaked@acme.iam.gserviceaccount.com"})

	_, err := auth.Authenticate(httptest.NewRequest("GET", "/v1/metrics", nil))
	if err == nil {
		t.Fatal("a revoked subject authenticated")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("the caller is not told why: %v", err)
	}
}

func TestAnUnrevokedSubjectStillWorks(t *testing.T) {
	path := writeList(t, `
version: 1
revoked:
  - subject: someone-else@acme.iam.gserviceaccount.com
`)
	auth := revoking(t, path, govern.Identity{Subject: "sa-fine@acme.iam.gserviceaccount.com"})

	if _, err := auth.Authenticate(httptest.NewRequest("GET", "/v1/metrics", nil)); err != nil {
		t.Errorf("an unrevoked caller was refused: %v", err)
	}
}

// TestOneCredentialCanBeRevokedWithoutTheWorkload.
//
// The lever that actually gets pulled. Revoking a subject takes the workload
// offline, so during an incident somebody weighs an outage against a leak
// and often waits. Revoking one jti costs nothing, which is why it has to
// exist.
func TestOneCredentialCanBeRevokedWithoutTheWorkload(t *testing.T) {
	path := writeList(t, `
version: 1
revoked:
  - jti: 7f3a91c4-leaked
    reason: one token pasted into a support ticket
`)
	list, err := govern.LoadRevocations(path)
	if err != nil {
		t.Fatal(err)
	}

	subject := "sa-agent@acme.iam.gserviceaccount.com"
	leaked := govern.Identity{Subject: subject, TokenID: "7f3a91c4-leaked"}
	other := govern.Identity{Subject: subject, TokenID: "0000-still-good"}

	if _, revoked := list.Check(leaked); !revoked {
		t.Error("the leaked credential was not revoked")
	}
	if _, revoked := list.Check(other); revoked {
		t.Error("revoking one token took the whole workload down with it")
	}
}

// TestARevocationTakesEffectWithoutARestart.
//
// The whole point. An operator edits the file and the next request is
// refused; nothing is restarted and nothing is redeployed.
func TestARevocationTakesEffectWithoutARestart(t *testing.T) {
	path := writeList(t, "version: 1\nrevoked: []\n")
	id := govern.Identity{Subject: "sa-agent@acme.iam.gserviceaccount.com"}
	auth := revoking(t, path, id)

	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	if _, err := auth.Authenticate(req); err != nil {
		t.Fatalf("refused before anything was revoked: %v", err)
	}

	// The incident. Written with a later modification time, because a file
	// rewritten inside one timestamp tick is exactly what a scripted
	// revocation looks like and the reload has to notice it anyway.
	body := "version: 1\nrevoked:\n  - subject: sa-agent@acme.iam.gserviceaccount.com\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}

	// Past the throttle, so the next check re-reads.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := auth.Authenticate(req); err != nil {
			return // revoked, without a restart
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("the credential was still accepted after being revoked in the file")
}

// TestAMissingFileAtStartupIsAnError.
//
// Treating a typo'd path as "nothing is revoked" is the failure where an
// operator believes revocation is configured and it silently is not.
func TestAMissingFileAtStartupIsAnError(t *testing.T) {
	if _, err := govern.LoadRevocations(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing revocation list loaded as an empty one")
	}
}

// TestTheListSurvivesTheFileGoingAway.
//
// Mid-edit the file may briefly be absent or unparseable. Clearing the list
// then would un-revoke a leaked credential at exactly the wrong moment, so
// the last good list keeps applying.
func TestTheListSurvivesTheFileGoingAway(t *testing.T) {
	path := writeList(t, `
version: 1
revoked:
  - subject: sa-leaked@acme.iam.gserviceaccount.com
`)
	list, err := govern.LoadRevocations(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	id := govern.Identity{Subject: "sa-leaked@acme.iam.gserviceaccount.com"}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, revoked := list.Check(id); !revoked {
			t.Fatal("deleting the file un-revoked a credential")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestAnEntryThatRevokesNothingIsRejected, because it reads as protection
// and provides none.
func TestAnEntryThatRevokesNothingIsRejected(t *testing.T) {
	_, err := govern.LoadRevocations(writeList(t, `
version: 1
revoked:
  - reason: we meant to put a subject here
`))
	if err == nil {
		t.Fatal("an entry with neither subject nor jti was accepted")
	}
	if !strings.Contains(err.Error(), "revokes nothing") {
		t.Errorf("the error is unclear: %v", err)
	}
}

// TestSubjectAndJTITogetherAreRejected.
//
// It reads as "this token, but only for this subject" and does not do that.
// Either half alone is unambiguous.
func TestSubjectAndJTITogetherAreRejected(t *testing.T) {
	_, err := govern.LoadRevocations(writeList(t, `
version: 1
revoked:
  - subject: sa@acme.example
    jti: abc-123
`))
	if err == nil {
		t.Fatal("an ambiguous entry was accepted")
	}
}

// TestAMisspelledKeyIsRejected. `subjects:` where `subject:` was meant would
// revoke nobody and report as configured.
func TestAMisspelledKeyIsRejected(t *testing.T) {
	_, err := govern.LoadRevocations(writeList(t, `
version: 1
revoked:
  - subjects: sa@acme.example
`))
	if err == nil {
		t.Fatal("a misspelled key was ignored rather than reported")
	}
}

// TestTheReasonIsNotReturnedToTheCaller.
//
// The reason is written for an operator and may name an incident or a
// person. What the caller can act on is that the credential is no longer
// valid.
func TestTheReasonIsNotReturnedToTheCaller(t *testing.T) {
	path := writeList(t, `
version: 1
revoked:
  - subject: sa-leaked@acme.iam.gserviceaccount.com
    reason: INCIDENT-4471, key exfiltrated by a former contractor
`)
	auth := revoking(t, path, govern.Identity{Subject: "sa-leaked@acme.iam.gserviceaccount.com"})

	_, err := auth.Authenticate(httptest.NewRequest("GET", "/v1/metrics", nil))
	if err == nil {
		t.Fatal("not revoked")
	}
	for _, leak := range []string{"INCIDENT-4471", "contractor", "exfiltrated"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the operator's note reached the caller: %v", err)
		}
	}
}

// TestSubjectMatchingIgnoresCase, because an identity provider is not
// consistent about it and a revocation that misses on capitalisation is a
// revocation that did not happen.
func TestSubjectMatchingIgnoresCase(t *testing.T) {
	path := writeList(t, `
version: 1
revoked:
  - subject: SA-Leaked@Acme.Example
`)
	list, err := govern.LoadRevocations(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, revoked := list.Check(govern.Identity{Subject: "sa-leaked@acme.example"}); !revoked {
		t.Error("a revocation was missed on capitalisation alone")
	}
}
