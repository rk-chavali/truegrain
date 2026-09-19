package rest_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
)

// The shared job store, against a real PostgreSQL.
//
// Against a real one because every property worth asserting here is a
// property of two connections seeing the same row, and a fake would agree
// with whatever this code believed.
//
// Needs TRUEGRAIN_TEST_POSTGRES_DSN. CI sets it from a service container,
// so these do not skip there, and the build fails on any skipped test.

func store(t *testing.T) *rest.PostgresJobStore {
	t.Helper()
	dsn := os.Getenv("TRUEGRAIN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TRUEGRAIN_TEST_POSTGRES_DSN is not set; see .github/workflows/ci.yml")
	}
	s, err := rest.NewPostgresJobStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting to the job store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// owner returns a name unique to this test, so tests that run in the same
// database do not see each other's jobs through the per-owner limit.
func owner(t *testing.T) string {
	return "sa-" + strings.ToLower(t.Name()) + "@test.example"
}

func sampleResult() *engine.Result {
	return &engine.Result{
		Columns:  []string{"region", "order_revenue"},
		Rows:     [][]any{{"NE", 750.0}, {"MW", 135.5}},
		RowCount: 2,
		Dialect:  "duckdb",
	}
}

// TestAJobSubmittedOnOneReplicaIsVisibleOnAnother.
//
// The whole reason a shared store exists. Two stores over the same database
// are two replicas: previously the second would have answered 404 and the
// caller would have concluded their job vanished.
func TestAJobSubmittedOnOneReplicaIsVisibleOnAnother(t *testing.T) {
	replicaA, replicaB := store(t), store(t)
	ctx := context.Background()
	who := owner(t)

	job, err := replicaA.Reserve(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := replicaA.Finish(ctx, job.ID, sampleResult(), nil); err != nil {
		t.Fatal(err)
	}

	got, found, err := replicaB.Get(ctx, job.ID, who)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a job submitted on one replica was invisible on another")
	}
	if got.State != "succeeded" {
		t.Errorf("state = %q, want succeeded", got.State)
	}
	if got.Result == nil || got.Result.RowCount != 2 {
		t.Errorf("the result did not survive the round trip: %+v", got.Result)
	}
	if got.Result.Rows[0][0] != "NE" {
		t.Errorf("rows came back wrong: %+v", got.Result.Rows)
	}
}

// TestAJobIsInvisibleToAnotherIdentity.
//
// A finished job holds rows only its owner was authorized to read, and the
// handler renders "not yours" and "does not exist" identically so the
// endpoint is not an oracle for guessing identifiers.
func TestAJobIsInvisibleToAnotherIdentity(t *testing.T) {
	s := store(t)
	ctx := context.Background()

	job, err := s.Reserve(ctx, owner(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, job.ID, sampleResult(), nil); err != nil {
		t.Fatal(err)
	}

	if _, found, err := s.Get(ctx, job.ID, "somebody-else@test.example"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("another identity could read a job's result")
	}
}

// TestTheRefusalSurvivesTheRoundTrip.
//
// The code, hint and retry class are the difference between a caller that
// rewrites its question and one that retries a denial forever. Flattening
// the error to a message would lose all three, which is why the store holds
// them as fields.
func TestTheRefusalSurvivesTheRoundTrip(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	who := owner(t)

	job, err := s.Reserve(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	refusal := &plan.Refusal{
		Code:   plan.CodeFanOut,
		Reason: "joining order_lines repeats every orders row",
		Hint:   "line_revenue answers this at the right grain",
	}
	if err := s.Finish(ctx, job.ID, nil, refusal); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.Get(ctx, job.ID, who)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.Failure == nil {
		t.Fatal("the failure was not stored")
	}
	if got.Failure.Code != plan.CodeFanOut {
		t.Errorf("code = %q, want %q", got.Failure.Code, plan.CodeFanOut)
	}
	if !strings.Contains(got.Failure.Hint, "line_revenue") {
		t.Errorf("the hint was lost: %q", got.Failure.Hint)
	}
	if got.Failure.Retry != string(plan.RetryModify) {
		t.Errorf("retry = %q, want modify", got.Failure.Retry)
	}
}

// TestAnExecutorErrorIsClassifiedRatherThanLost.
//
// A warehouse failing is not a refusal: the request was legal. Later is the
// honest class, and a caller that read it as "never" would give up on a
// question that will work in a minute.
func TestAnExecutorErrorIsClassifiedRatherThanLost(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	who := owner(t)

	job, err := s.Reserve(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, job.ID, nil, errors.New("connection reset")); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.Get(ctx, job.ID, who)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failure == nil || got.Failure.Retry != string(plan.RetryLater) {
		t.Errorf("an executor error was not classified as retryable later: %+v", got.Failure)
	}
}

// TestTheConcurrencyLimitHoldsAcrossReplicas.
//
// The case a shared store introduces. Counting and inserting separately
// would let a caller at their limit submit twice by submitting to two
// replicas at once, so it is one statement.
func TestTheConcurrencyLimitHoldsAcrossReplicas(t *testing.T) {
	replicaA, replicaB := store(t), store(t)
	ctx := context.Background()
	who := owner(t)

	// Fill the allowance on one replica.
	var ids []string
	for {
		job, err := replicaA.Reserve(ctx, who)
		if err != nil {
			break
		}
		ids = append(ids, job.ID)
		if len(ids) > 32 {
			t.Fatal("the per-caller limit never refused")
		}
	}
	if len(ids) == 0 {
		t.Fatal("the first reservation was refused")
	}

	// The other replica has to refuse too, or the limit is per-process and
	// therefore not a limit.
	_, err := replicaB.Reserve(ctx, who)
	if err == nil {
		t.Fatal("a second replica let the same caller past their limit")
	}
	var refusal *plan.Refusal
	if !errors.As(err, &refusal) || refusal.Code != rest.CodeTooManyJobs {
		t.Errorf("got %v, want a too_many_jobs refusal", err)
	}

	for _, id := range ids {
		_ = replicaA.Finish(ctx, id, sampleResult(), nil)
	}
}

// TestCancellingFromAnotherReplicaMarksItCancelled.
//
// The honest half of the boundary: the job is cancelled everywhere, so the
// caller stops waiting. Stopping the query itself only works on the replica
// running it, which JobStore documents.
func TestCancellingFromAnotherReplicaMarksItCancelled(t *testing.T) {
	replicaA, replicaB := store(t), store(t)
	ctx := context.Background()
	who := owner(t)

	job, err := replicaA.Reserve(ctx, who)
	if err != nil {
		t.Fatal(err)
	}

	got, found, err := replicaB.Cancel(ctx, job.ID, who)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", got.State)
	}

	// And the query finishing afterwards must not overwrite it, or a caller
	// who cancelled would see a result they said they did not want.
	if err := replicaA.Finish(ctx, job.ID, sampleResult(), nil); err != nil {
		t.Fatal(err)
	}
	after, _, err := replicaA.Get(ctx, job.ID, who)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "cancelled" {
		t.Errorf("state = %q after the query returned; a cancellation was overwritten",
			after.State)
	}
}

// TestCancellingIsIdempotent, because a client retrying a DELETE is normal
// and should not get an error for it.
func TestCancellingIsIdempotent(t *testing.T) {
	s := store(t)
	ctx := context.Background()
	who := owner(t)

	job, err := s.Reserve(ctx, who)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		got, found, err := s.Cancel(ctx, job.ID, who)
		if err != nil || !found {
			t.Fatalf("cancel %d: found=%v err=%v", i+1, found, err)
		}
		if got.State != "cancelled" {
			t.Errorf("cancel %d: state = %q", i+1, got.State)
		}
	}
}

// TestAnOversizedResultFailsRatherThanTruncates.
//
// A shortened result set is a wrong answer that looks like a right one,
// which is the single thing this project exists to not produce. The caller
// is told the size so they can narrow the question.
func TestAnOversizedResultFailsRatherThanTruncates(t *testing.T) {
	s := store(t)
	s.SetMaxResultBytes(512) // small, so the fixture does not have to be huge
	ctx := context.Background()
	who := owner(t)

	job, err := s.Reserve(ctx, who)
	if err != nil {
		t.Fatal(err)
	}

	big := &engine.Result{Columns: []string{"padding"}}
	for range 200 {
		big.Rows = append(big.Rows, []any{strings.Repeat("x", 64)})
	}
	if err := s.Finish(ctx, job.ID, big, nil); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.Get(ctx, job.ID, who)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "failed" {
		t.Fatalf("state = %q, want failed rather than a truncated result", got.State)
	}
	if got.Result != nil {
		t.Error("a partial result was stored alongside the failure")
	}
	if got.Failure == nil || !strings.Contains(got.Failure.Reason, "512") {
		t.Errorf("the failure does not name the limit: %+v", got.Failure)
	}
	if got.Failure != nil && !strings.Contains(got.Failure.Hint, "narrow") {
		t.Errorf("the failure does not say what to do: %q", got.Failure.Hint)
	}
}

// TestTheStoreReportsThatItIsShared, because health tells an operator
// whether this deployment can run more than one replica and that answer has
// to come from the store rather than from a flag somebody set separately.
func TestTheStoreReportsThatItIsShared(t *testing.T) {
	if !store(t).Durable() {
		t.Error("the Postgres store reports itself as not durable")
	}
	if memory := rest.NewMemoryJobStore(); memory.Durable() {
		t.Error("the in-memory store reports itself as durable")
	}
}
