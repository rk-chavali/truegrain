package rest

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Running doctor on a schedule.
//
// Drift is found by looking regularly, not by looking once. Somebody drops a
// column on a Tuesday and nothing notices until a caller gets an error on
// Thursday, which is the most expensive place to find out. `truegrain doctor`
// was written to be cheap enough to run on a timer for exactly this reason,
// and until now the timer had to be somebody else's cron.
//
// In process rather than a job queue, deliberately. This runs one metadata
// query every half hour against a warehouse the process is already connected
// to; a scheduler with a store, a leader election and a retry policy would be
// more machinery than the work it does. When two replicas both run it they
// both record their own history, which is the honest answer for a check whose
// result is about the connection it was run over.
//
// What this does not do is tell anybody. There is no delivery here: no email,
// no webhook, no page. Drift shows up on the Checks screen when somebody
// looks, which is worth having on its own and is a smaller promise than an
// alert nobody configured a destination for.

// maxDoctorRuns bounds the history. At the default interval this is about a day,
// which is the window in which "when did this start" is still answerable.
const maxDoctorRuns = 48

// DoctorRun is one scheduled check.
type DoctorRun struct {
	At       time.Time `json:"at"`
	OK       bool      `json:"ok"`
	Checked  int       `json:"tables_checked"`
	Findings int       `json:"findings"`
	// Error is set when the check could not run at all, which is different
	// from running and finding something wrong.
	Error string `json:"error,omitempty"`
	// ModelVersion ties the result to what was being served, so a run from
	// before a reload is not read as evidence about the model after it.
	ModelVersion string `json:"model_version"`
}

type doctorHistory struct {
	mu   sync.Mutex
	runs []DoctorRun
	// every is the configured interval, reported so a reader can tell a
	// gap from a check that simply has not come round yet.
	every time.Duration
}

func (h *doctorHistory) record(run DoctorRun) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = append(h.runs, run)
	if len(h.runs) > maxDoctorRuns {
		h.runs = h.runs[len(h.runs)-maxDoctorRuns:]
	}
}

func (h *doctorHistory) recent() []DoctorRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]DoctorRun, len(h.runs))
	copy(out, h.runs)
	return out
}

/*
WithDoctorSchedule checks the warehouse every interval until ctx is done.

The first check runs immediately rather than after one interval. An operator
who starts the server wants to know now whether the model matches the
warehouse, and waiting half an hour to find out the connection is wrong is the
behaviour nobody asks for twice.
*/
func (s *Server) WithDoctorSchedule(ctx context.Context, every time.Duration, lg *slog.Logger) *Server {
	if every <= 0 {
		return s
	}
	s.doctorRuns = &doctorHistory{every: every}

	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			s.runScheduledDoctor(ctx, lg)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

// runScheduledDoctor performs one check.
//
// The engine is read here rather than closed over, because a reload swaps it
// and a check pinned to whatever was serving at startup would be reporting on
// a model nobody is using.
func (s *Server) runScheduledDoctor(ctx context.Context, lg *slog.Logger) {
	eng := s.engine()
	if eng == nil {
		return
	}

	// Bounded independently of the interval. A warehouse that has stopped
	// answering must not leave this goroutine parked on a socket until the
	// process exits.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	run := DoctorRun{At: time.Now().UTC(), ModelVersion: eng.ModelVersion()}
	report, err := eng.Doctor(ctx)
	if err != nil {
		run.Error = err.Error()
		if lg != nil {
			// Warn rather than error: the model is still being served, and
			// a warehouse that cannot be reached for a metadata check is a
			// thing to look at rather than an outage.
			lg.Warn("the scheduled warehouse check could not run",
				slog.String("error", err.Error()))
		}
		s.doctorRuns.record(run)
		return
	}

	run.OK = report.OK()
	run.Checked = report.Checked
	run.Findings = len(report.Findings)
	s.doctorRuns.record(run)

	if !run.OK && lg != nil {
		lg.Warn("the warehouse no longer matches the model",
			slog.Int("findings", run.Findings),
			slog.String("model_version", run.ModelVersion))
	}
}

// doctorHistoryHandler serves what the schedule has seen.
//
// 404 when nothing is scheduled, like every other capability here: an engine
// checking nothing has no history, as opposed to a history in which nothing
// went wrong.
func (s *Server) doctorHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireScope(w, r, ScopeReadModel); !ok {
		return
	}
	if s.doctorRuns == nil {
		writeError(w, http.StatusNotFound, "doctor_not_scheduled",
			"this engine does not check the warehouse on a schedule",
			"start it with -doctor-every, for example 30m")
		return
	}

	runs := s.doctorRuns.recent()
	drifted := 0
	for _, run := range runs {
		if !run.OK && run.Error == "" {
			drifted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"every_seconds": int(s.doctorRuns.every.Seconds()),
		"runs":          runs,
		"drifted":       drifted,
	})
}
