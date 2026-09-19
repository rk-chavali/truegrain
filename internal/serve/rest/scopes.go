package rest

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// What a credential may do, as opposed to what an identity may read.
//
// These are different questions and only one of them was being asked. Groups
// and policy decide which columns are legible to a person or a service. A
// scope decides whether this particular token may execute a query at all, and
// until now every token could: a continuous integration job that only
// validates a model held a credential that could also read every row its
// identity was allowed. The blast radius of leaking it was the warehouse
// rather than the model metadata it actually needed.
//
// Two scopes, not a taxonomy. The line worth drawing is between reading what
// the model says and making the warehouse do work, because that is the line
// between a cheap secret and an expensive one. More scopes can be added when
// somebody has a use for them; a permission system nobody can hold in their
// head gets configured wrong.

const (
	// ScopeReadModel lists metrics, dimensions, namespaces and health, and
	// compiles SQL without running it.
	ScopeReadModel = "read:model"
	// ScopeRunQuery executes against the warehouse, synchronously or as a job.
	ScopeRunQuery = "run:query"
	// ScopeDeploy tells the engine to look at its repository again.
	//
	// Its own scope and never implied by the others, because it is a different
	// kind of authority: reading and querying answer for the caller, and this
	// changes what every caller is answered with. A deploy credential leaking
	// is a different incident from a read credential leaking, so it is a
	// different credential.
	ScopeDeploy = "deploy:model"
)

// Scopes lists every scope this build understands, which is what an operator
// needs when writing the flag.
func Scopes() []string { return []string{ScopeReadModel, ScopeRunQuery, ScopeDeploy} }

// CodeOutOfScope is a credential that is genuinely this caller's and is not
// permitted to do this.
const CodeOutOfScope = "out_of_scope"

func init() {
	// Never: the request is fine and the credential is not. Retrying with a
	// different request will not help, and an agent should say so rather than
	// try the same call with fewer metrics.
	plan.RegisterRetry(CodeOutOfScope, plan.RetryNever)
}

// ParseScopes reads an operator's comma separated list.
//
// An unknown scope is dropped rather than ignored silently at the point of
// use: a typo in a flag would otherwise produce a credential that is narrower
// than intended and fails somewhere far from the mistake. The caller reports
// what was dropped.
func ParseScopes(raw string) (scopes []string, unknown []string) {
	for _, part := range strings.Split(raw, ",") {
		scope := strings.TrimSpace(part)
		if scope == "" {
			continue
		}
		if !slices.Contains(Scopes(), scope) {
			unknown = append(unknown, scope)
			continue
		}
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return scopes, unknown
}

// requireScope resolves the caller and checks the credential may do this.
//
// 403 rather than 401: the credential was accepted and is not permitted, which
// is a different thing from not being recognised, and telling a caller to
// re-authenticate when a new token would not help wastes everybody's time.
func (s *Server) requireScope(w http.ResponseWriter, r *http.Request, scope string) (govern.Identity, bool) {
	id, ok := s.identify(w, r)
	if !ok {
		return govern.Identity{}, false
	}
	if !id.Can(scope) {
		writeError(w, http.StatusForbidden, CodeOutOfScope,
			"this credential is not permitted to "+describeScope(scope),
			"it holds "+strings.Join(id.Scopes, ", ")+
				"; use a credential with "+scope+", or ask an operator to widen this one")
		return govern.Identity{}, false
	}
	return id, true
}

// describeScope says what was refused in terms of what the caller was doing,
// rather than repeating the scope name back at them.
func describeScope(scope string) string {
	switch scope {
	case ScopeRunQuery:
		return "run queries against the warehouse"
	case ScopeReadModel:
		return "read the model"
	}
	return scope
}
