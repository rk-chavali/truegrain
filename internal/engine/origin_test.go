package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
)

// Where a model came from.
//
// The engine behaves identically whichever way a model arrived, so there is no
// behaviour here to test. What there is instead is a question an operator
// cannot otherwise answer: is the thing serving the thing we merged. Without
// this the answer involves ssh and a directory listing, and by then the
// disputed number is a week old.

func originEngine(t *testing.T, origin engine.Origin) *engine.Engine {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath: "../../testdata/workspace",
		Dialect:   "duckdb",
		Origin:    origin,
		Resolver:  govern.AllowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestHealthReportsTheCommitBeingServed(t *testing.T) {
	const commit = "4a2a2bbee5608b40553c900a77f951ba5dc3a9ed"
	h := originEngine(t, engine.Origin{
		Repository: "https://github.com/acme/models.git",
		Ref:        "main",
		Commit:     commit,
		Subdir:     "models",
	}).Health()

	if h.Origin == nil {
		t.Fatal("a model followed from a repository must say so")
	}
	// The commit is the field worth reading. A ref moves; this is what
	// answered, and it is what a disputed number is traced back to.
	if h.Origin.Commit != commit {
		t.Errorf("Commit = %q, want %q", h.Origin.Commit, commit)
	}
	if h.Origin.Ref != "main" || h.Origin.Subdir != "models" {
		t.Errorf("the rest of the origin was lost: %+v", h.Origin)
	}
}

// TestAModelReadFromAPathHasNoOrigin, rather than an empty object. A reader
// meeting `"origin": {}` has to work out whether it means "from a path" or
// "from a repository we failed to identify", and those want different actions.
func TestAModelReadFromAPathHasNoOrigin(t *testing.T) {
	h := originEngine(t, engine.Origin{}).Health()
	if h.Origin != nil {
		t.Errorf("a model read from a path reported an origin: %+v", h.Origin)
	}

	// And the field is absent from the response rather than null, because a
	// generated client turns a present null into a populated optional.
	body, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "origin") {
		t.Errorf("the origin key is in the response when there is none:\n%s", body)
	}
}

// TestTheOriginNeverCarriesACredential.
//
// gitsync refuses a URL with one in it, so this should be impossible. It is
// asserted anyway because health is public to every authenticated caller, and
// a credential reaching it would be a leak through the one endpoint people are
// told to read.
func TestTheOriginNeverCarriesACredential(t *testing.T) {
	h := originEngine(t, engine.Origin{
		Repository: "https://github.com/acme/models.git",
		Commit:     "abc1234",
	}).Health()

	// Scoped to the origin rather than the whole response, which legitimately
	// contains an `@` in every namespace owner.
	body, err := json.Marshal(h.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "@") {
		t.Errorf("the reported origin contains userinfo:\n%s", body)
	}
}

func TestShortRendersACommitTheWayPeopleQuoteOne(t *testing.T) {
	full := engine.Origin{Commit: "4a2a2bbee5608b40553c900a77f951ba5dc3a9ed"}
	if got := full.Short(); got != "4a2a2bb" {
		t.Errorf("Short() = %q, want 4a2a2bb", got)
	}
	// A short or absent commit must not panic on the slice.
	if got := (engine.Origin{Commit: "abc"}).Short(); got != "abc" {
		t.Errorf("Short() on a short commit = %q", got)
	}
	if got := (engine.Origin{}).Short(); got != "" {
		t.Errorf("Short() on no commit = %q", got)
	}
}
