package rest

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Asynchronous execution exists because a warehouse query can outlive an agent.
//
// A BigQuery scan over a year of partitions routinely runs longer than the
// default HTTP timeout of every agent framework in use. Synchronously, the
// client gives up at thirty seconds and retries, the warehouse runs the query
// twice, and the caller is billed twice for an answer nobody reads. Submitting
// returns an identifier immediately and the caller polls.
//
// MCP deliberately does not get this. An MCP client is a local process talking
// over stdio with no request timeout to outlive, so a fifth tool would be cost
// with no benefit, and the tool surface is fixed at four by test.

// Refusal codes this file adds.
const (
	CodeTooManyJobs = "too_many_jobs"
	CodeJobNotFound = "job_not_found"
	CodeBadCursor   = "invalid_cursor"
	CodeNoExecutor  = "no_executor"
)

func init() {
	// Backpressure is the one refusal here worth repeating unchanged: the
	// caller's slots free up as their own jobs finish.
	plan.RegisterRetry(CodeTooManyJobs, plan.RetryLater)
	plan.RegisterRetry(CodeJobNotFound, plan.RetryNever)
	plan.RegisterRetry(CodeBadCursor, plan.RetryModify)
	plan.RegisterRetry(CodeNoExecutor, plan.RetryNever)
}

type jobState string

const (
	jobRunning   jobState = "running"
	jobSucceeded jobState = "succeeded"
	jobFailed    jobState = "failed"
	jobCancelled jobState = "cancelled"
)

func (s jobState) terminal() bool { return s != jobRunning }

// Defaults for the job store. They bound memory, because a finished job pins
// its entire result set until it is collected or expires.
const (
	defaultJobTTL      = 10 * time.Minute
	defaultJobRuntime  = 30 * time.Minute
	defaultMaxRunning  = 4
	defaultJobPageSize = 1000
	maxJobPageSize     = 10000
)

// submitJob compiles a request, then runs it in the background.
//
// Compilation, and therefore the governance gate, happens synchronously here.
// A request the caller may not make never becomes a job: it comes back as a
// refusal on this call, so an agent modifies it immediately instead of polling
// to discover it was denied. It also means a 202 is a promise the query was
// authorized, not merely that it was accepted.
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeRunQuery)
	if !ok {
		return
	}
	req, ok := s.decodeRequest(w, r)
	if !ok {
		return
	}
	if !s.engine().CanExecute() {
		writeError(w, http.StatusUnprocessableEntity, CodeNoExecutor,
			"this engine is configured to compile but not execute, so there is nothing to run",
			"use POST /v1/compile to see the SQL")
		return
	}

	c, err := s.engine().Compile(r.Context(), id, req)
	if err != nil {
		writeRefusal(w, err)
		return
	}

	// The job outlives this request, so it cannot use this request's context:
	// that one is cancelled the moment the handler returns.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), defaultJobRuntime)
	j, err := s.jobs.Reserve(r.Context(), id.Subject)
	if err != nil {
		cancel()
		writeRefusal(w, err)
		return
	}
	// The cancel function stays in this process, because that is the only
	// place it means anything. The store holds what every replica can act on.
	s.running.Store(j.ID, cancel)

	// Query recompiles, which re-runs the gate immediately before execution. A
	// grant revoked between this 202 and the query starting fails the job
	// rather than returning rows.
	go func() {
		defer func() {
			s.running.Delete(j.ID)
			cancel()
		}()
		res, qerr := s.engine().Query(ctx, id, req)
		// Its own context: the job's is about to be cancelled, and a shared
		// store needs a live one to record the result through.
		done, release := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer release()
		if err := s.jobs.Finish(done, j.ID, res, qerr); err != nil {
			if s.log != nil {
				s.log.Error("recording a finished job failed",
					"job_id", j.ID, "error", err.Error())
			}
		}
	}()

	w.Header().Set("Location", "/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":        j.ID,
		"state":         string(jobRunning),
		"submitted_at":  millis(j.Created).Format(time.RFC3339),
		"compiled_sql":  c.SQL,
		"columns":       c.Columns,
		"namespace":     c.Namespace,
		"model_version": c.ModelVersion,
		"dialect":       c.Dialect,
		"parts":         c.Parts,
	})
}

// getJob reports a job's state and, once it has succeeded, a page of rows.
//
// Polling a failed job is a 200 carrying state "failed": the poll itself
// succeeded, and the failure is described in the body with the same code, hint
// and retry class a synchronous refusal would have carried. Returning the
// refusal's own status here would leave a caller unable to distinguish "your
// query was rejected" from "your poll was rejected".
func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeRunQuery)
	if !ok {
		return
	}
	j, found, err := s.jobs.Get(r.Context(), r.PathValue("id"), id.Subject)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "job_store_unavailable",
			"the job store could not be reached", "retry shortly")
		return
	}
	if !found {
		writeJobNotFound(w)
		return
	}

	body := map[string]any{
		"job_id":       j.ID,
		"state":        string(j.State),
		"submitted_at": millis(j.Created).Format(time.RFC3339),
	}
	if j.State.terminal() {
		body["finished_at"] = millis(j.Finished).Format(time.RFC3339)
		body["duration_ms"] = j.Finished - j.Created
	} else {
		body["elapsed_ms"] = time.Since(millis(j.Created)).Milliseconds()
	}

	switch j.State {
	case jobFailed:
		// The parts are stored rather than a message, so the code, hint and
		// retry class survive a shared store as well as an in-memory one.
		if f := j.Failure; f != nil {
			body["code"], body["reason"], body["hint"] = f.Code, f.Reason, f.Hint
			body["retry"] = f.Retry
		}
	case jobSucceeded:
		page, err := pageRows(j.Result, r.URL.Query())
		if err != nil {
			writeRefusal(w, err)
			return
		}
		maps.Copy(body, page)
	}
	writeJSON(w, http.StatusOK, body)
}

// cancelJob stops a running query.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireScope(w, r, ScopeRunQuery)
	if !ok {
		return
	}
	j, found, err := s.jobs.Cancel(r.Context(), r.PathValue("id"), id.Subject)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "job_store_unavailable",
			"the job store could not be reached", "retry shortly")
		return
	}
	if !found {
		writeJobNotFound(w)
		return
	}
	// Stop the query too, if this is the replica running it. On any other
	// replica the job is now cancelled and the caller stops waiting, but the
	// warehouse keeps going until its own deadline: see JobStore.
	if cancel, ok := s.running.Load(j.ID); ok {
		cancel.(context.CancelFunc)()
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": j.ID, "state": string(j.State)})
}

func writeJobNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, CodeJobNotFound,
		"no such job for this caller",
		"a job is visible only to the identity that submitted it, and is discarded once it expires")
}

// pageRows slices a finished result according to cursor and page_size.
func pageRows(res *engine.Result, q url.Values) (map[string]any, error) {
	size := defaultJobPageSize
	if raw := q.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxJobPageSize {
			return nil, &plan.Refusal{Code: plan.CodeBadLimit,
				Reason: fmt.Sprintf("page_size must be a whole number between 1 and %d, got %q",
					maxJobPageSize, raw)}
		}
		size = n
	}

	offset := 0
	if raw := q.Get("cursor"); raw != "" {
		n, err := decodeCursor(raw)
		if err != nil || n < 0 || n > len(res.Rows) {
			return nil, &plan.Refusal{Code: CodeBadCursor,
				Reason: "the cursor is not one this job issued",
				Hint:   "omit cursor for the first page, or use the next_cursor from the previous one"}
		}
		offset = n
	}

	end := min(offset+size, len(res.Rows))
	body := map[string]any{
		"columns":       res.Columns,
		"rows":          res.Rows[offset:end],
		"row_count":     len(res.Rows),
		"compiled_sql":  res.CompiledSQL,
		"namespace":     res.Namespace,
		"model_version": res.ModelVersion,
		"dialect":       res.Dialect,
	}
	if end < len(res.Rows) {
		body["next_cursor"] = encodeCursor(end)
	}
	return body, nil
}

// The cursor is opaque so paging can become a real streaming cursor later
// without changing the wire contract.
//
// ponytail: an offset over a materialized slice is all it needs to be today.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(s string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(raw))
}

// jobDurabilityNote is the one thing `health` was silent about.
//
// The engine's own health report covers what governance does and does not
// enforce, and it cannot mention this: the job store lives here, not in the
// engine. So a self-hoster could read a clean report, put two replicas behind
// a load balancer, submit to one and poll the other, and get a 404 that looks
// like the product losing their query.
//
// Said on the health endpoint rather than only in the README, because the
// contract of this endpoint is that an operator does not have to already know
// what to ask.
const jobDurabilityNote = "Asynchronous jobs are held in this process only. " +
	"A restart loses every job that has not finished, and a second replica cannot " +
	"report on a job this one accepted: a caller polling the wrong replica is told " +
	"the job does not exist. Run one replica, or route each caller to the same one."

// notes reads the enforcement notes back out of a marshalled health report.
//
// They arrive as []any from the round trip through JSON, and dropping them
// because of that would quietly delete every governance caveat the engine
// reported, which is the opposite of what this endpoint is for.
func notes(body map[string]any) []string {
	existing, _ := body["enforcement_notes"].([]any)
	out := make([]string, 0, len(existing)+1)
	for _, n := range existing {
		if s, ok := n.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// millis turns a stored timestamp back into a time.
//
// Stored as Unix milliseconds because a row holds a number and not a
// time.Time, and because two replicas comparing timestamps need one
// representation rather than two.
func millis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// jobNote says what this deployment's job store means for replicas.
func (s *Server) jobNote() string {
	if s.jobs.Durable() {
		return "Asynchronous jobs are shared between replicas, so this deployment " +
			"can run more than one. Cancelling a job still only stops the query on " +
			"the replica running it: on any other, the job is marked cancelled and " +
			"the caller stops waiting, but the warehouse continues until the " +
			"query's own deadline."
	}
	return jobDurabilityNote
}
