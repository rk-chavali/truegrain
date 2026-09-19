package rest_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// Asking the engine to re-read its model source.
//
// The endpoint exists so a pipeline does not wait for the next poll. It
// carries no model, which is the design rather than an omission: a route that
// accepted one would be a second way into production, skipping the pull
// request, the checks and the diff that says which numbers move.

// deployServer returns a handler whose reload is wired to a counter.
func deployServer(t *testing.T, scopes []string, fail error) (http.Handler, *int32) {
	t.Helper()
	var calls int32
	srv := server(t, false, govern.Identity{Subject: "ci@acme.com", Scopes: scopes}).
		WithReload(func() error {
			atomic.AddInt32(&calls, 1)
			return fail
		})
	return srv.Handler(), &calls
}

func TestADeployCredentialCanAskForAReload(t *testing.T) {
	h, calls := deployServer(t, []string{rest.ScopeDeploy}, nil)

	res := do(t, h, "POST", "/v1/reload", "", token)
	if res.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", res.Code, res.Body)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("the reload was asked for %d times, want 1", got)
	}
	// The reply must not claim a commit. A sync is not instant, and naming one
	// before the swap happened is a lie a pipeline would then assert as fact.
	if strings.Contains(res.Body.String(), "commit\":\"") {
		t.Errorf("the reply claims a commit it cannot know yet: %s", res.Body)
	}
}

// TestReadingOrQueryingIsNotDeploying.
//
// The point of a separate scope. A credential that can read the model, or even
// make the warehouse do work, is answering for its own caller. Deploying
// changes what every other caller is answered with, so a leak of one is a
// different incident from a leak of the other.
func TestReadingOrQueryingIsNotDeploying(t *testing.T) {
	for _, scopes := range [][]string{
		{rest.ScopeReadModel},
		{rest.ScopeRunQuery},
		{rest.ScopeReadModel, rest.ScopeRunQuery},
	} {
		h, calls := deployServer(t, scopes, nil)

		res := do(t, h, "POST", "/v1/reload", "", token)
		if res.Code != http.StatusForbidden {
			t.Errorf("%v was allowed to deploy, got %d: %s", scopes, res.Code, res.Body)
		}
		if got := atomic.LoadInt32(calls); got != 0 {
			t.Errorf("%v triggered %d reloads", scopes, got)
		}
	}
}

// TestAnEngineFollowingNothingSaysThereIsNothingToReload.
//
// 404 rather than 403: the capability is absent, not withheld. An engine
// reading a model from a path has nothing to re-read, and accepting the call
// would let a pipeline believe it had deployed.
func TestAnEngineFollowingNothingSaysThereIsNothingToReload(t *testing.T) {
	h := server(t, false, govern.Identity{
		Subject: "ci@acme.com",
		Scopes:  []string{rest.ScopeDeploy},
	}).Handler()

	res := do(t, h, "POST", "/v1/reload", "", token)
	if res.Code != http.StatusNotFound {
		t.Fatalf("want 404 when nothing is followed, got %d: %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "git+") {
		t.Errorf("the reply should say how to make it follow one: %s", res.Body)
	}
}

// TestAFailedSyncSaysThePreviousModelIsStillServing.
//
// The distinction a caller most needs: this is a failed deploy, not a broken
// engine. Reporting it as a plain failure would send somebody to restart a
// process that is answering correctly.
func TestAFailedSyncSaysThePreviousModelIsStillServing(t *testing.T) {
	h, _ := deployServer(t, []string{rest.ScopeDeploy}, errors.New("the repository could not be reached"))

	res := do(t, h, "POST", "/v1/reload", "", token)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("want 502 on a failed sync, got %d: %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "still being served") {
		t.Errorf("the reply should say the previous model still answers: %s", res.Body)
	}
}

// TestThereIsNoWayToPostAModel.
//
// Guards the property the whole design rests on. If a body ever starts being
// read here, git stops being the only source of truth and a deploy credential
// becomes a way to put unreviewed SQL in front of every caller.
func TestThereIsNoWayToPostAModel(t *testing.T) {
	h, calls := deployServer(t, []string{rest.ScopeDeploy}, nil)

	before := currentDigest(t, h)
	model := `{"models":"semantic_model:\n  - name: sneaky\n    metrics: []\n"}`
	res := do(t, h, "POST", "/v1/reload", model, token)

	// The body is ignored, not rejected: accepting it would be worse, and
	// rejecting it would imply a shape exists that would have worked.
	if res.Code != http.StatusAccepted {
		t.Fatalf("want the body ignored, got %d: %s", res.Code, res.Body)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Error("the reload was not the thing that happened")
	}
	if after := currentDigest(t, h); after != before {
		t.Errorf("a posted model changed what is served: %s then %s", before, after)
	}
}

func currentDigest(t *testing.T, h http.Handler) string {
	t.Helper()
	res := do(t, h, "GET", "/v1/model/version", "", token)
	var body struct {
		ModelVersion string `json:"model_version"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.ModelVersion
}
