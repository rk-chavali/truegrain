package rest_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// blockingExecutor stands in for a warehouse that is slow, so a job is
// observably running rather than finishing before the test can look at it.
//
// It returns synthetic rows rather than reaching a database: these tests are
// about the job lifecycle and who may read a result, not about SQL.
type blockingExecutor struct {
	release chan struct{} // closed to let every pending Execute return
	rows    int
	fail    error
}

func (b *blockingExecutor) Name() string { return "blocking (test)" }

func (b *blockingExecutor) Execute(ctx context.Context, _ govern.Identity, _ string, _ []any) (*engine.Rows, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		// A cancelled job must not be reported as a success.
		return nil, ctx.Err()
	}
	if b.fail != nil {
		return nil, b.fail
	}
	rows := make([][]any, b.rows)
	for i := range rows {
		rows[i] = []any{fmt.Sprintf("region-%d", i), i * 10}
	}
	return &engine.Rows{Columns: []string{"region", "order_revenue"}, Rows: rows, JobID: "test-job"}, nil
}

func (b *blockingExecutor) Close() error { return nil }

// jobHandler builds a server whose executor the test controls. tokens maps a
// bearer token to the identity it authenticates as, so one handler can serve
// two different callers.
func jobHandler(t *testing.T, ex engine.Executor, tokens map[string]govern.Identity) http.Handler {
	t.Helper()
	modelPath := filepath.Join("..", "..", "..", "testdata", "models", "retail.yaml")
	ws, err := workspace.Load(modelPath, workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		ModelPath: modelPath, Dialect: "duckdb",
		Resolver: govern.AllowAll{}, Executor: ex,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rest.New(eng, rest.StaticTokens{Tokens: tokens}).Handler()
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding response: %v\n%s", err, raw)
	}
	return body
}

const queryBody = `{"metrics":["order_revenue"],"dimensions":["customers.region"]}`

// submit posts a job and returns its identifier.
func submit(t *testing.T, h http.Handler, bearer, body string) string {
	t.Helper()
	w := do(t, h, "POST", "/v1/jobs", body, bearer)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202 from a submitted job, got %d: %s", w.Code, w.Body)
	}
	id, _ := decode(t, w.Body.Bytes())["job_id"].(string)
	if id == "" {
		t.Fatalf("a submitted job returned no job_id: %s", w.Body)
	}
	return id
}

// awaitState polls until the job leaves the running state.
func awaitState(t *testing.T, h http.Handler, bearer, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := do(t, h, "GET", "/v1/jobs/"+id, "", bearer)
		if w.Code != http.StatusOK {
			t.Fatalf("polling a job: want 200, got %d: %s", w.Code, w.Body)
		}
		body := decode(t, w.Body.Bytes())
		if body["state"] != "running" {
			return body
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s never left the running state", id)
	return nil
}

// TestJobRunsAndReturnsRows is the happy path: submit, poll, collect.
func TestJobRunsAndReturnsRows(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), rows: 3}
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	w := do(t, h, "POST", "/v1/jobs", queryBody, token)
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body)
	}
	accepted := decode(t, w.Body.Bytes())

	// The SQL comes back on submit, so an agent can show its work without
	// waiting for the warehouse.
	if sql, _ := accepted["compiled_sql"].(string); sql == "" {
		t.Error("a submitted job must return the compiled SQL immediately")
	}
	if accepted["state"] != "running" {
		t.Errorf("want state running on submit, got %v", accepted["state"])
	}
	id := accepted["job_id"].(string)

	// Still running while the executor is held.
	body := decode(t, do(t, h, "GET", "/v1/jobs/"+id, "", token).Body.Bytes())
	if body["state"] != "running" {
		t.Fatalf("want state running while the executor is blocked, got %v", body["state"])
	}
	if _, hasRows := body["rows"]; hasRows {
		t.Error("a running job must not return rows")
	}

	close(ex.release)
	done := awaitState(t, h, token, id)
	if done["state"] != "succeeded" {
		t.Fatalf("want succeeded, got %v: %v", done["state"], done)
	}
	rows, _ := done["rows"].([]any)
	if len(rows) != 3 {
		t.Errorf("want 3 rows, got %d", len(rows))
	}
	if done["model_version"] == nil || done["compiled_sql"] == nil {
		t.Error("a finished job must carry the model version and SQL that produced it")
	}
}

// TestJobIsInvisibleToAnotherIdentity is the access control on a finished
// result. A job holds rows its owner was authorized to read; a second caller
// polling the identifier would be a governance bypass with no gate in its path.
func TestJobIsInvisibleToAnotherIdentity(t *testing.T) {
	const otherToken = "second-caller-token-not-a-real-secret"
	ex := &blockingExecutor{release: make(chan struct{}), rows: 2}
	h := jobHandler(t, ex, map[string]govern.Identity{
		token:      {Subject: "owner@example.com"},
		otherToken: {Subject: "intruder@example.com"},
	})

	id := submit(t, h, token, queryBody)
	close(ex.release)
	awaitState(t, h, token, id)

	w := do(t, h, "GET", "/v1/jobs/"+id, "", otherToken)
	// 404 rather than 403: a 403 would confirm the identifier is real, which
	// turns the endpoint into an oracle for guessing job identifiers.
	if w.Code != http.StatusNotFound {
		t.Fatalf("another identity read the job: want 404, got %d: %s", w.Code, w.Body)
	}
	if body := w.Body.String(); jsonContains(body, "region") {
		t.Errorf("the 404 leaked result content:\n%s", body)
	}

	// Cancelling somebody else's job is the same boundary.
	if got := do(t, h, "DELETE", "/v1/jobs/"+id, "", otherToken).Code; got != http.StatusNotFound {
		t.Errorf("another identity cancelled the job: want 404, got %d", got)
	}
}

func jsonContains(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 &&
		json.Valid([]byte(haystack)) && contains(haystack, needle)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestDeniedRequestNeverBecomesAJob is why compilation is synchronous. If the
// gate ran inside the goroutine, a denial would arrive only after a poll, and
// a 202 would mean nothing about authorization.
func TestDeniedRequestNeverBecomesAJob(t *testing.T) {
	modelPath := filepath.Join("..", "..", "..", "testdata", "models", "retail.yaml")
	ws, err := workspace.Load(modelPath, workspace.Options{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := govern.LoadFilePolicy(filepath.Join("..", "..", "..", "testdata", "policy.yaml"), ws)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		ModelPath: modelPath, Dialect: "duckdb", Resolver: pol,
		Executor: &blockingExecutor{release: make(chan struct{})},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := rest.New(eng, rest.StaticTokens{Tokens: map[string]govern.Identity{
		token: {Subject: "sa-denied@example.iam.gserviceaccount.com"},
	}}).Handler()

	w := do(t, h, "POST", "/v1/jobs", `{"metrics":["order_revenue"]}`, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a denied request must be refused on submit, not asynchronously: got %d: %s",
			w.Code, w.Body)
	}
	body := w.Body.String()
	if contains(body, "SELECT") {
		t.Errorf("a denied submit leaked SQL:\n%s", body)
	}
	if contains(body, "job_id") {
		t.Errorf("a denied request became a job:\n%s", body)
	}
}

// TestJobPaginates covers the reason the cursor exists: a result that would
// blow an agent's context if returned whole.
func TestJobPaginates(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), rows: 25}
	close(ex.release)
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	id := submit(t, h, token, queryBody)
	awaitState(t, h, token, id)

	seen := 0
	cursor := ""
	for range 10 {
		path := "/v1/jobs/" + id + "?page_size=10"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		body := decode(t, do(t, h, "GET", path, "", token).Body.Bytes())

		// row_count is the size of the whole result, not of the page, so a
		// caller knows how much is left before walking it.
		if got := body["row_count"]; got != float64(25) {
			t.Fatalf("want row_count 25 on every page, got %v", got)
		}
		rows, _ := body["rows"].([]any)
		seen += len(rows)

		next, _ := body["next_cursor"].(string)
		if next == "" {
			if len(rows) != 5 {
				t.Errorf("want 5 rows on the last page, got %d", len(rows))
			}
			break
		}
		if len(rows) != 10 {
			t.Errorf("want 10 rows on a full page, got %d", len(rows))
		}
		cursor = next
	}
	if seen != 25 {
		t.Errorf("paging returned %d of 25 rows", seen)
	}

	// A cursor the job did not issue is refused rather than silently clamped,
	// which would return a page the caller did not ask for.
	w := do(t, h, "GET", "/v1/jobs/"+id+"?cursor=bm90LWEtY3Vyc29y", "", token)
	if w.Code == http.StatusOK {
		t.Errorf("a forged cursor was accepted: %s", w.Body)
	}
}

// TestJobConcurrencyIsCapped is the quota. Without it an agent in a loop pins
// unbounded memory and unbounded warehouse spend.
func TestJobConcurrencyIsCapped(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), rows: 1}
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	for range 4 {
		submit(t, h, token, queryBody)
	}
	w := do(t, h, "POST", "/v1/jobs", queryBody, token)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusTooManyRequests {
		t.Fatalf("the fifth concurrent job was accepted: got %d: %s", w.Code, w.Body)
	}
	body := decode(t, w.Body.Bytes())
	if body["code"] != rest.CodeTooManyJobs {
		t.Errorf("want code %q, got %v", rest.CodeTooManyJobs, body["code"])
	}
	// The caller's own jobs will free the slots, so this is the one refusal
	// worth repeating unchanged.
	if body["retry"] != "later" {
		t.Errorf("backpressure must classify as retry later, got %v", body["retry"])
	}

	close(ex.release)
}

// TestJobCancelStops covers a caller abandoning an expensive query.
func TestJobCancelStops(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), rows: 1}
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	id := submit(t, h, token, queryBody)
	w := do(t, h, "DELETE", "/v1/jobs/"+id, "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 from cancel, got %d: %s", w.Code, w.Body)
	}
	if got := decode(t, w.Body.Bytes())["state"]; got != "cancelled" {
		t.Fatalf("want state cancelled, got %v", got)
	}

	// The executor's context is cancelled, so it returns an error. That must
	// not overwrite the cancellation with a failure.
	body := decode(t, do(t, h, "GET", "/v1/jobs/"+id, "", token).Body.Bytes())
	if body["state"] != "cancelled" {
		t.Errorf("a cancelled job reported %v after its executor returned", body["state"])
	}

	// Cancelling twice is not an error.
	if got := do(t, h, "DELETE", "/v1/jobs/"+id, "", token).Code; got != http.StatusOK {
		t.Errorf("cancel must be idempotent, second call got %d", got)
	}
	close(ex.release)
}

// TestFailedJobCarriesRetryClass: a poll that finds a failure is a successful
// poll, so it is 200, and the failure is described in the body with the same
// vocabulary a synchronous refusal uses.
func TestFailedJobCarriesRetryClass(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), fail: errors.New("warehouse unavailable")}
	close(ex.release)
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	id := submit(t, h, token, queryBody)
	body := awaitState(t, h, token, id)
	if body["state"] != "failed" {
		t.Fatalf("want failed, got %v", body["state"])
	}
	if body["retry"] != "later" {
		t.Errorf("an executor failure must classify as retry later, got %v", body["retry"])
	}
	if _, hasRows := body["rows"]; hasRows {
		t.Error("a failed job must not return rows")
	}
}

func TestJobsRequireAuth(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), rows: 1}
	close(ex.release)
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	cases := []struct{ method, path, body string }{
		{"POST", "/v1/jobs", queryBody},
		{"GET", "/v1/jobs/whatever", ""},
		{"DELETE", "/v1/jobs/whatever", ""},
	}
	for _, c := range cases {
		if got := do(t, h, c.method, c.path, c.body, "").Code; got != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", c.method, c.path, got)
		}
	}
}

// TestJobsRejectSQLInBody applies the raw-SQL guarantee to the newest endpoint.
func TestJobsRejectSQLInBody(t *testing.T) {
	ex := &blockingExecutor{release: make(chan struct{}), rows: 1}
	close(ex.release)
	h := jobHandler(t, ex, map[string]govern.Identity{token: {Subject: "tester"}})

	for _, body := range []string{
		`{"sql":"SELECT 1"}`,
		`{"metrics":["order_revenue"],"sql":"SELECT 1"}`,
	} {
		if got := do(t, h, "POST", "/v1/jobs", body, token).Code; got != http.StatusBadRequest {
			t.Errorf("a job body carrying SQL must be rejected, got %d for %s", got, body)
		}
	}
}

// TestHealthAdmitsJobsAreNodeLocal.
//
// The endpoint's contract is that an operator does not have to already know
// what to ask. Someone evaluating this for self-hosting reads the health
// report, sees nothing about durability, runs two replicas behind a load
// balancer, and finds out when a caller polls the wrong one and is told their
// job does not exist.
func TestHealthAdmitsJobsAreNodeLocal(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})

	rec := do(t, h, "GET", "/v1/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health returned %d", rec.Code)
	}

	var body struct {
		EnforcementNotes []string `json:"enforcement_notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	var said bool
	for _, n := range body.EnforcementNotes {
		if strings.Contains(n, "held in this process only") {
			said = true
		}
	}
	if !said {
		t.Errorf("health does not admit jobs are node-local:\n%v", body.EnforcementNotes)
	}
}

// TestHealthStillCarriesTheEngineNotes. Appending to the list is one bad line
// away from replacing it, and the notes being replaced are every governance
// caveat the engine reported.
func TestHealthStillCarriesTheEngineNotes(t *testing.T) {
	h := handler(t, false, govern.Identity{Subject: "tester"})

	rec := do(t, h, "GET", "/v1/health", "", "")
	var body struct {
		EnforcementNotes []string `json:"enforcement_notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	// This server is built without a policy, so the engine reports that no
	// column-level control exists. Losing that would be a governance claim
	// silently upgraded.
	var said bool
	for _, n := range body.EnforcementNotes {
		if strings.Contains(n, "No column-level access control") {
			said = true
		}
	}
	if !said {
		t.Errorf("the engine's own notes were dropped:\n%v", body.EnforcementNotes)
	}
}
