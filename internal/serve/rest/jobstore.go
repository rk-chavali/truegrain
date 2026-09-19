package rest

import (
	"context"
	"errors"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Where running and finished jobs live.
//
// One in-memory map was the whole implementation, which meant one replica:
// a caller that submitted to /v1/jobs and polled could be routed to a
// process that had never heard of their job and get a 404. /v1/health said
// so, which is better than not saying so and still a cap on how this
// deploys.
//
// # What a shared store can and cannot do
//
// It can make a job visible from every replica, which is what lifts the
// one-replica cap: submit on A, poll on B, collect on C.
//
// It cannot cancel a query running on another replica, and pretending
// otherwise would be the dishonest part. A context.CancelFunc is a pointer
// in one process's memory; there is no way to reach it from another. So
// cancelling a job owned by a different replica marks it cancelled, which
// stops the caller waiting and discards the result, and the warehouse query
// keeps running until it finishes or hits its own deadline. That is written
// down here, reported by health, and is the honest boundary rather than a
// bug to find later.
//
// Cancelling a job on the replica running it does stop the query, which is
// the common case: a caller usually polls the connection they submitted on.
type JobStore interface {
	// Reserve registers a running job, or refuses when the owner is at their
	// concurrency limit.
	//
	// It does not take a cancel function. Cancellation is node-local by
	// nature, so the running replica keeps its own and the store keeps only
	// what every replica can act on.
	Reserve(ctx context.Context, owner string) (JobRecord, error)

	// Finish records a terminal state. It is a no-op for a job already in
	// one, so a cancellation is not overwritten by the driver error that
	// cancelling it produced, and a job cancelled from another replica stays
	// cancelled when the query it was running eventually returns.
	Finish(ctx context.Context, id string, res *engine.Result, err error) error

	// Get returns a job only to the identity that created it.
	//
	// The second return is false both for a job that does not exist and for
	// one owned by somebody else, and the handler renders both as 404.
	// Distinguishing them would turn the endpoint into an oracle for guessing
	// job identifiers, and a finished job holds rows only its owner was
	// authorized to read.
	Get(ctx context.Context, id, owner string) (JobRecord, bool, error)

	// Cancel marks a job cancelled. Idempotent: cancelling a finished job
	// returns its existing state rather than an error.
	Cancel(ctx context.Context, id, owner string) (JobRecord, bool, error)

	// Name describes the store for health, so an operator can tell whether
	// this deployment can run more than one replica.
	Name() string

	// Durable reports whether jobs survive this process, which is what
	// decides whether the replica warning applies.
	Durable() bool

	Close() error
}

// JobRecord is one job as any store represents it.
//
// Deliberately free of anything process-local. Everything here can be
// written to a row and read back on another replica, which is the property
// that makes a shared store possible at all.
type JobRecord struct {
	ID    string
	Owner string
	State jobState

	Created  int64 // Unix milliseconds, because a row stores a timestamp and not a time.Time
	Finished int64

	Result *engine.Result
	// Failure describes why a job failed, in parts rather than as a message.
	//
	// An error does not survive a round trip through a database, and
	// flattening it to a string would lose the code, the hint and the retry
	// class. Those are the whole difference between a caller that knows to
	// rewrite its question and one that retries a governance denial forever,
	// so they are stored as fields and reassembled.
	Failure *Failure
}

// Failure is a job's terminal error, in a shape a row can hold.
type Failure struct {
	Code   string
	Reason string
	Hint   string
	Retry  string
}

// failureOf decomposes an error for storage.
func failureOf(err error) *Failure {
	if err == nil {
		return nil
	}
	var r *plan.Refusal
	if errors.As(err, &r) {
		return &Failure{
			Code: r.Code, Reason: r.Reason, Hint: r.Hint, Retry: string(r.Retry()),
		}
	}
	// An executor failure is not a refusal: the request was legal and the
	// warehouse failed. Later is the honest classification, and it is the
	// same one the Go SDK applies to the same case.
	return &Failure{
		Code:   "execution_failed",
		Reason: err.Error(),
		Retry:  string(plan.RetryLater),
	}
}

// Err rebuilds the error a caller sees.
func (f *Failure) Err() error {
	if f == nil {
		return nil
	}
	return &plan.Refusal{Code: f.Code, Reason: f.Reason, Hint: f.Hint}
}
