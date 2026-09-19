package rest

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Revoking wraps an authenticator with a deny list.
//
// Wrapping rather than building the check into each authenticator, because
// there are four of them and a deny list that one forgot to consult would be
// documented as enforced and missing on the path somebody uses. The list and
// the rule live in govern, because the wire protocol needs the same ones and
// authenticates differently.
type Revoking struct {
	Inner Authenticator
	List  *govern.Revocations
}

// Authenticate verifies the credential, then checks whether it has been
// turned off.
//
// After verification rather than before, so an unauthenticated caller cannot
// probe the deny list by watching which subjects produce a different error.
func (r Revoking) Authenticate(req *http.Request) (govern.Identity, error) {
	id, err := r.Inner.Authenticate(req)
	if err != nil {
		return govern.Identity{}, err
	}
	if _, revoked := r.List.Check(id); revoked {
		// The reason stays out of the response: it is written for an operator
		// and may name an incident or a person. What the caller can act on is
		// that the credential is no longer valid.
		return govern.Identity{}, errors.New("this credential has been revoked")
	}
	return id, nil
}

// Name describes the configuration for health.
func (r Revoking) Name() string {
	inner := "unknown"
	if n, ok := r.Inner.(interface{ Name() string }); ok {
		inner = n.Name()
	}
	return fmt.Sprintf("%s, with %d revocation(s)", inner, r.List.Count())
}
