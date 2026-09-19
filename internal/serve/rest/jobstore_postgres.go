package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/observe"
)

// PostgresJobStore shares jobs between replicas.
//
// This is what lifts the one-replica cap: submit on A, poll on B, collect
// on C. Read JobStore first for what it still cannot do, which is stop a
// query running in another process.
//
// # Why PostgreSQL and not something faster
//
// Because a deployment that has this engine very likely already has one,
// and a job store is not a hot path: a poll every second or two per running
// job, against a table that holds minutes of history. Adding Redis would be
// a second thing to run, back up and secure for a workload measured in
// single-digit queries per second.
//
// It is deliberately a separate database from the warehouse. The warehouse
// holds the customer's data and this holds the engine's own state; pointing
// them at the same place would mean the engine writing to something it
// otherwise only ever reads, which is a property worth keeping.
//
// # Bounding what a row can hold
//
// A finished job pins its whole result. In memory that is bounded by the
// per-caller job limit and a short TTL; in a table it is bounded by
// whatever the largest result happens to be, and a hundred thousand rows of
// JSON is not a row anybody wants. A result past the cap fails the job with
// a refusal naming the limit, rather than being truncated, because a
// silently shortened result set is a wrong answer and this whole project
// exists to not give one.
type PostgresJobStore struct {
	pool *pgxpool.Pool

	ttl        time.Duration
	maxRunning int
	maxResult  int
	now        func() time.Time
}

// defaultMaxJobResultBytes bounds one stored result.
//
// 32MB of JSON is roughly a hundred thousand narrow rows, which is already
// past plan.MaxLimit for anything but the widest result. Past this the job
// fails and says so.
const defaultMaxJobResultBytes = 32 << 20

// jobTable is created on connect.
//
// Created here rather than shipped as a migration because it is one table
// owned entirely by this process, and asking somebody to run a migration
// tool to turn on a flag is how the flag does not get turned on. It is
// idempotent, so every replica can run it at startup.
const jobTable = `
CREATE TABLE IF NOT EXISTS truegrain_jobs (
	id            TEXT PRIMARY KEY,
	owner         TEXT NOT NULL,
	state         TEXT NOT NULL,
	created_ms    BIGINT NOT NULL,
	finished_ms   BIGINT NOT NULL DEFAULT 0,
	result        JSONB,
	failure       JSONB
);
CREATE INDEX IF NOT EXISTS truegrain_jobs_owner_state
	ON truegrain_jobs (owner, state);
CREATE INDEX IF NOT EXISTS truegrain_jobs_finished
	ON truegrain_jobs (finished_ms) WHERE state <> 'running';
`

// NewPostgresJobStore connects and ensures the table exists.
//
// Eagerly, so a bad DSN fails at startup with one clear error rather than
// on the first job somebody submits.
func NewPostgresJobStore(ctx context.Context, dsn string) (*PostgresJobStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx redacts the password in its own errors. Nothing here should put
		// the DSN into a message by hand and undo that.
		return nil, fmt.Errorf("the job store connection string could not be parsed: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to the job store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to the job store: %w", err)
	}
	if _, err := pool.Exec(ctx, jobTable); err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating the job table: %w", err)
	}
	return &PostgresJobStore{
		pool:       pool,
		ttl:        defaultJobTTL,
		maxRunning: defaultMaxRunning,
		maxResult:  defaultMaxJobResultBytes,
		now:        time.Now,
	}, nil
}

func (s *PostgresJobStore) Name() string {
	return "postgres (shared between replicas)"
}
func (s *PostgresJobStore) Durable() bool { return true }
func (s *PostgresJobStore) Close() error  { s.pool.Close(); return nil }

func (s *PostgresJobStore) Reserve(ctx context.Context, owner string) (JobRecord, error) {
	if err := s.sweep(ctx); err != nil {
		return JobRecord{}, err
	}

	id, err := newJobID()
	if err != nil {
		return JobRecord{}, err
	}
	now := s.now().UnixMilli()

	// One statement, so the count and the insert cannot race two replicas
	// past the limit between them. A SELECT then an INSERT would let a
	// caller at the limit submit twice by submitting to two replicas at
	// once, which is exactly the case a shared store introduces.
	const q = `
		INSERT INTO truegrain_jobs (id, owner, state, created_ms)
		SELECT $1, $2, 'running', $3
		WHERE (
			SELECT count(*) FROM truegrain_jobs
			WHERE owner = $2 AND state = 'running'
		) < $4
		RETURNING id`

	var inserted string
	err = s.pool.QueryRow(ctx, q, id, owner, now, s.maxRunning).Scan(&inserted)
	if err == pgx.ErrNoRows {
		return JobRecord{}, tooManyJobs(s.maxRunning)
	}
	if err != nil {
		return JobRecord{}, fmt.Errorf("reserving a job: %w", err)
	}

	observe.JobStarted()
	return JobRecord{ID: id, Owner: owner, State: jobRunning, Created: now}, nil
}

func (s *PostgresJobStore) Finish(ctx context.Context, id string, res *engine.Result, err error) error {
	state := jobSucceeded
	var resultJSON, failureJSON []byte

	if err != nil {
		state = jobFailed
		failureJSON, _ = json.Marshal(failureOf(err))
	} else if res != nil {
		encoded, merr := json.Marshal(res)
		switch {
		case merr != nil:
			state = jobFailed
			failureJSON, _ = json.Marshal(&Failure{
				Code:   "result_not_storable",
				Reason: "the result could not be encoded for the shared job store: " + merr.Error(),
				Retry:  "never",
			})
		case len(encoded) > s.maxResult:
			// Failed rather than truncated. A shortened result set is a wrong
			// answer that looks like a right one, and the caller can act on
			// being told the size.
			state = jobFailed
			failureJSON, _ = json.Marshal(&Failure{
				Code: "result_too_large",
				Reason: fmt.Sprintf(
					"the result is %d bytes encoded, over this deployment's %d byte "+
						"limit for a shared job", len(encoded), s.maxResult),
				Hint: "narrow the question with a filter or a smaller limit, or run it " +
					"synchronously with POST /v1/query, which streams to one caller " +
					"and stores nothing",
				Retry: "modify",
			})
		default:
			resultJSON = encoded
		}
	}

	// WHERE state = 'running' is what makes this a no-op on a job already
	// terminal, so a cancellation is not overwritten by the driver error
	// that cancelling it produced, and a job cancelled from another replica
	// stays cancelled when its query eventually returns.
	const q = `
		UPDATE truegrain_jobs
		SET state = $2, finished_ms = $3, result = $4, failure = $5
		WHERE id = $1 AND state = 'running'`

	tag, qerr := s.pool.Exec(ctx, q, id, string(state), s.now().UnixMilli(), resultJSON, failureJSON)
	if qerr != nil {
		return fmt.Errorf("recording a finished job: %w", qerr)
	}
	if tag.RowsAffected() > 0 {
		observe.JobFinished()
	}
	return nil
}

func (s *PostgresJobStore) Get(ctx context.Context, id, owner string) (JobRecord, bool, error) {
	// Owner in the WHERE rather than checked after reading: a job belonging
	// to somebody else must not be read at all, not read and then discarded.
	const q = `
		SELECT id, owner, state, created_ms, finished_ms, result, failure
		FROM truegrain_jobs
		WHERE id = $1 AND owner = $2`

	return s.scanOne(ctx, q, id, owner)
}

func (s *PostgresJobStore) Cancel(ctx context.Context, id, owner string) (JobRecord, bool, error) {
	// Conditional update then read, in one round trip. Cancelling a finished
	// job returns its existing state rather than an error, so this is
	// idempotent the way a client retrying a DELETE needs.
	// The statement reports whether it was this call that cancelled, rather
	// than the caller inferring it from the state afterwards. Two replicas
	// cancelling the same job must decrement the running gauge once, and
	// comparing timestamps to guess which one won is the kind of check that
	// is quietly wrong for a year.
	// The UPDATE returns the new row, and the second branch reads the old one
	// only when the update did not fire.
	//
	// It has to be this way round. A data-modifying CTE's changes are not
	// visible to the rest of the same statement in PostgreSQL: a plain SELECT
	// beside the UPDATE reads the snapshot from before it and reports the job
	// as still running, which is what the first version of this did.
	const q = `
		WITH updated AS (
			UPDATE truegrain_jobs
			SET state = 'cancelled', finished_ms = $3
			WHERE id = $1 AND owner = $2 AND state = 'running'
			RETURNING id, owner, state, created_ms, finished_ms, result, failure
		)
		SELECT id, owner, state, created_ms, finished_ms, result, failure, true
		FROM updated
		UNION ALL
		SELECT j.id, j.owner, j.state, j.created_ms, j.finished_ms, j.result, j.failure, false
		FROM truegrain_jobs j
		WHERE j.id = $1 AND j.owner = $2 AND NOT EXISTS (SELECT 1 FROM updated)`

	var (
		rec           JobRecord
		state         string
		resultJSON    []byte
		failureJSON   []byte
		justCancelled bool
	)
	err := s.pool.QueryRow(ctx, q, id, owner, s.now().UnixMilli()).Scan(
		&rec.ID, &rec.Owner, &state, &rec.Created, &rec.Finished,
		&resultJSON, &failureJSON, &justCancelled)
	if err == pgx.ErrNoRows {
		return JobRecord{}, false, nil
	}
	if err != nil {
		return JobRecord{}, false, fmt.Errorf("cancelling a job: %w", err)
	}
	rec.State = jobState(state)
	if justCancelled {
		observe.JobFinished()
	}
	return rec, true, nil
}

func (s *PostgresJobStore) scanOne(ctx context.Context, q string, args ...any) (JobRecord, bool, error) {
	var (
		rec         JobRecord
		state       string
		resultJSON  []byte
		failureJSON []byte
	)
	err := s.pool.QueryRow(ctx, q, args...).Scan(
		&rec.ID, &rec.Owner, &state, &rec.Created, &rec.Finished, &resultJSON, &failureJSON)
	if err == pgx.ErrNoRows {
		return JobRecord{}, false, nil
	}
	if err != nil {
		return JobRecord{}, false, fmt.Errorf("reading a job: %w", err)
	}
	rec.State = jobState(state)

	if len(resultJSON) > 0 {
		var res engine.Result
		if err := json.Unmarshal(resultJSON, &res); err != nil {
			return JobRecord{}, false, fmt.Errorf("decoding a stored result: %w", err)
		}
		rec.Result = &res
	}
	if len(failureJSON) > 0 {
		var f Failure
		if err := json.Unmarshal(failureJSON, &f); err != nil {
			return JobRecord{}, false, fmt.Errorf("decoding a stored failure: %w", err)
		}
		rec.Failure = &f
	}
	return rec, true, nil
}

// sweep drops expired jobs.
//
// On reserve only, rather than on every read. A poll is the frequent
// operation and does not need to pay for it, and a row lingering a few
// seconds past its TTL costs nothing. A running job is never swept on age:
// it is stopped by its own deadline, and until then it is doing work.
func (s *PostgresJobStore) sweep(ctx context.Context) error {
	const q = `DELETE FROM truegrain_jobs WHERE state <> 'running' AND finished_ms < $1`
	cutoff := s.now().Add(-s.ttl).UnixMilli()
	if _, err := s.pool.Exec(ctx, q, cutoff); err != nil {
		return fmt.Errorf("sweeping expired jobs: %w", err)
	}
	return nil
}

// SetMaxResultBytes bounds one stored result.
//
// Exported so a deployment with a different appetite can say so, and so a
// test can prove the bound without building a 32MB fixture. The default is
// what a deployment gets by not thinking about it.
func (s *PostgresJobStore) SetMaxResultBytes(n int) { s.maxResult = n }
