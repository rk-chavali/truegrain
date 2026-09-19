package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/rollup"
)

// Deciding whether a pre-aggregated table is current enough to read.
//
// The planner establishes that a rollup could answer a question. This
// establishes that it should, which is a different claim and the one with
// the real risk behind it: a rollup that stopped building last Tuesday
// still covers every question it ever covered, and every answer it gives
// is wrong in a way no caller can see.
//
// So a rollup is read only when its declared freshness column is inside its
// declared max_age, checked against the warehouse. Three consequences worth
// stating rather than discovering.
//
// An engine with no executor never routes. It has nothing to check with,
// and compiled output that depended on unverified state would be a
// different statement depending on when it was produced.
//
// A failed check falls back to the base fact rather than refusing. A rollup
// is an optimisation, and turning a broken rollup build into an outage for
// questions the source tables answer perfectly well is the wrong trade. The
// operator finds out from the log and from health, not from their users.
//
// The check is cached, because doing it per query would add a round trip to
// every request to save one. The cache is short relative to the rollup's
// own max_age, so a rollup that goes stale is noticed within a fraction of
// the window it was allowed.

// freshness caches what is known about each rollup's currency.
type freshness struct {
	mu      sync.Mutex
	checked map[string]freshnessResult
}

type freshnessResult struct {
	at    time.Time
	fresh bool
	// age is what the last successful probe measured, for health. Zero when
	// the probe itself failed.
	age time.Duration
	// err is why the last probe could not decide, if it could not. A rollup
	// whose freshness cannot be established is not read, and an operator
	// needs to see the reason rather than a rollup that silently never
	// helps.
	err error
}

// probeInterval is how long a freshness answer is reused.
//
// A tenth of the rollup's own allowance, so a table that goes stale is
// noticed early in the window rather than at the end of it, clamped at both
// ends: never more often than every five seconds, because that would put a
// round trip in front of a burst of queries, and never less often than
// every five minutes, because a daily rollup would otherwise be trusted for
// hours after it broke.
func probeInterval(maxAge time.Duration) time.Duration {
	d := maxAge / 10
	if d < 5*time.Second {
		return 5 * time.Second
	}
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}

// usableRollups returns the candidates that are current enough to read.
//
// Order is preserved, so the planner's preference (narrowest first) still
// decides which one is used when several are fresh.
func (e *Engine) usableRollups(ctx context.Context, candidates []*rollup.Rollup) []*rollup.Rollup {
	if len(candidates) == 0 || e.exec == nil {
		return nil
	}
	var out []*rollup.Rollup
	for _, r := range candidates {
		if e.isFresh(ctx, r) {
			out = append(out, r)
		}
	}
	return out
}

// freshnessProbeIdentity is who the freshness check runs as.
//
// The engine's own credentials, not the caller's, for two reasons. The
// answer is cached and shared, so a caller who cannot read the rollup
// table would otherwise mark it stale for everybody. And the probe reads
// one build timestamp, which is not anybody's data: there is nothing here
// for a caller's grants to protect. Where the executor impersonates and
// this identity has no warehouse role, the probe fails, the rollup is not
// read, and every query is answered from the base fact, which is the
// direction to fail in.
var freshnessProbeIdentity = govern.Identity{Subject: "truegrain.rollup-freshness"}

func (e *Engine) isFresh(ctx context.Context, r *rollup.Rollup) bool {
	e.rollupFreshness.mu.Lock()
	cached, ok := e.rollupFreshness.checked[r.Name]
	e.rollupFreshness.mu.Unlock()
	if ok && time.Since(cached.at) < probeInterval(r.Freshness.Age) {
		return cached.fresh
	}

	age, err := e.probeAge(ctx, r)
	result := freshnessResult{at: time.Now(), age: age, err: err}
	result.fresh = err == nil && age <= r.Freshness.Age

	e.rollupFreshness.mu.Lock()
	if e.rollupFreshness.checked == nil {
		e.rollupFreshness.checked = map[string]freshnessResult{}
	}
	e.rollupFreshness.checked[r.Name] = result
	e.rollupFreshness.mu.Unlock()
	return result.fresh
}

// probeAge asks the warehouse how old the rollup's newest row is.
//
// The statement is built from the rollup's declared source and column,
// which came from a file an operator wrote rather than from a caller, and
// both are quoted through the dialect and were checked at load to be plain
// identifiers. Nothing from a request reaches this text.
func (e *Engine) probeAge(ctx context.Context, r *rollup.Rollup) (time.Duration, error) {
	sql := fmt.Sprintf("SELECT MAX(%s) FROM %s",
		e.dialect.QuoteIdent(r.Freshness.Column), e.dialect.QuoteSource(r.Source))

	rows, err := e.exec.Execute(ctx, freshnessProbeIdentity, sql, nil)
	if err != nil {
		return 0, fmt.Errorf("checking rollup %s: %w", r.Name, err)
	}
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 1 {
		return 0, fmt.Errorf("checking rollup %s: the probe returned no value", r.Name)
	}

	newest, ok := asTime(rows.Rows[0][0])
	if !ok {
		// An empty rollup returns null here, which is not an error and is
		// also not fresh: a table with no rows answers every question with
		// nothing.
		return 0, fmt.Errorf(
			"rollup %s has no rows, or %s does not hold a timestamp",
			r.Name, r.Freshness.Column)
	}
	return time.Since(newest), nil
}

// asTime reads whatever the executor returned for a timestamp column.
//
// Executors differ: pgx returns time.Time, the DuckDB CLI returns text.
// Anything this cannot read is reported as unreadable rather than guessed,
// because a guess here decides whether a stale table is trusted.
func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05.999999999 -0700 MST",
			"2006-01-02 15:04:05.999999",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

// RollupHealth describes one declared rollup and what is known about it.
type RollupHealth struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Source     string `json:"source"`
	Metrics    int    `json:"metrics"`
	Dimensions int    `json:"dimensions"`
	MaxAge     string `json:"max_age"`
	// Checked is false before anything has probed this rollup, which is the
	// state it stays in when no query has yet been able to use it.
	Checked bool `json:"checked"`
	// Fresh is the last probe's verdict. An operator reading false here has
	// found a broken rollup build without anybody filing a ticket about a
	// slow dashboard.
	Fresh bool `json:"fresh,omitempty"`
	// AgeSeconds is what the last probe measured.
	AgeSeconds int64 `json:"age_seconds,omitempty"`
	// Error is why the last probe could not decide.
	Error string `json:"error,omitempty"`
}

// Rollups reports every declared rollup and the last thing learned about
// it, for health.
//
// Reported even when none has been probed. A rollup that exists and is
// never used is a table somebody is paying to build, and the difference
// between "not fresh" and "never checked" is the difference between a
// broken build and a rollup no query has ever matched.
func (e *Engine) Rollups() []RollupHealth {
	var out []RollupHealth
	for _, ns := range e.ws.Available() {
		for _, r := range ns.Rollups {
			h := RollupHealth{
				Name:       r.Name,
				Namespace:  ns.Name,
				Source:     r.Source,
				Metrics:    len(r.Metrics),
				Dimensions: len(r.Dimensions),
				MaxAge:     r.Freshness.MaxAge,
			}
			e.rollupFreshness.mu.Lock()
			cached, ok := e.rollupFreshness.checked[r.Name]
			e.rollupFreshness.mu.Unlock()
			if ok {
				h.Checked = true
				h.Fresh = cached.fresh
				h.AgeSeconds = int64(cached.age.Seconds())
				if cached.err != nil {
					h.Error = cached.err.Error()
				}
			}
			out = append(out, h)
		}
	}
	return out
}
