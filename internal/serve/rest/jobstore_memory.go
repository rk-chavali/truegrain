package rest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/observe"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// MemoryJobStore holds jobs in this process.
//
// The default, and the right one for a single replica: it needs no
// database, loses nothing that matters on restart because a job is minutes
// old at most, and has no network in the path of a poll.
//
// Its limit is the one /v1/health reports: a job submitted here is invisible
// to every other replica. See PostgresJobStore for the shared version and
// for what a shared version still cannot do.
//
// ponytail: one mutex guards the store and every job's mutable state. Jobs
// are few and every critical section is a map operation; the slow part, the
// query itself, runs outside it.
type MemoryJobStore struct {
	mu   sync.Mutex
	jobs map[string]*JobRecord

	ttl        time.Duration
	maxRuntime time.Duration
	maxRunning int
	now        func() time.Time
}

// NewMemoryJobStore builds the default store.
func NewMemoryJobStore() *MemoryJobStore {
	return &MemoryJobStore{
		jobs:       map[string]*JobRecord{},
		ttl:        defaultJobTTL,
		maxRuntime: defaultJobRuntime,
		maxRunning: defaultMaxRunning,
		now:        time.Now,
	}
}

func (s *MemoryJobStore) Name() string  { return "in-memory (this process only)" }
func (s *MemoryJobStore) Durable() bool { return false }
func (s *MemoryJobStore) Close() error  { return nil }

// newJobID is 128 bits from crypto/rand rather than a counter. A guessable
// identifier would be the whole of the access control on a finished result.
func newJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a job id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *MemoryJobStore) Reserve(_ context.Context, owner string) (JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	running := 0
	for _, j := range s.jobs {
		if j.Owner == owner && j.State == jobRunning {
			running++
		}
	}
	if running >= s.maxRunning {
		return JobRecord{}, tooManyJobs(running)
	}

	id, err := newJobID()
	if err != nil {
		return JobRecord{}, err
	}
	j := &JobRecord{
		ID: id, Owner: owner, State: jobRunning,
		Created: s.now().UnixMilli(),
	}
	s.jobs[id] = j
	// Paired with the decrement in the two places a job can reach a terminal
	// state, both under this lock. A decrement left to a caller is a
	// decrement eventually forgotten on an error path.
	observe.JobStarted()
	return *j, nil
}

func (s *MemoryJobStore) Finish(_ context.Context, id string, res *engine.Result, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.State.terminal() {
		return nil
	}
	j.Finished = s.now().UnixMilli()
	observe.JobFinished()
	if err != nil {
		j.State, j.Failure = jobFailed, failureOf(err)
		return nil
	}
	j.State, j.Result = jobSucceeded, res
	return nil
}

func (s *MemoryJobStore) Get(_ context.Context, id, owner string) (JobRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	j, ok := s.jobs[id]
	if !ok || j.Owner != owner {
		return JobRecord{}, false, nil
	}
	return *j, true, nil
}

func (s *MemoryJobStore) Cancel(_ context.Context, id, owner string) (JobRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.Owner != owner {
		return JobRecord{}, false, nil
	}
	if j.State == jobRunning {
		j.State = jobCancelled
		j.Finished = s.now().UnixMilli()
		observe.JobFinished()
	}
	return *j, true, nil
}

// sweepLocked drops expired jobs.
//
// ponytail: swept on access rather than by a background goroutine, so the
// server has nothing extra to shut down and an idle process does no work.
// The cost is that a result is held until somebody touches the store, which
// the per-identity cap already bounds.
func (s *MemoryJobStore) sweepLocked() {
	cutoff := s.now().Add(-s.ttl).UnixMilli()
	for id, j := range s.jobs {
		// A running job is never swept on age: it is stopped by its own
		// context deadline, and until then it is still doing work.
		if j.State.terminal() && j.Finished < cutoff {
			delete(s.jobs, id)
		}
	}
}

// tooManyJobs is the backpressure refusal, shared by every store so the two
// cannot drift into saying different things about the same limit.
func tooManyJobs(running int) error {
	return &plan.Refusal{
		Code: CodeTooManyJobs,
		Reason: fmt.Sprintf(
			"this caller already has %d queries running, which is the limit", running),
		Hint: "wait for one to finish, or cancel one with DELETE /v1/jobs/{id}",
	}
}
