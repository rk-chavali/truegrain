package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rk-chavali/truegrain/internal/modeltest"
)

// `truegrain test` asserts what a model answers.
//
// `validate` says the model holds together and `diff` says a number changed.
// Neither says a number was ever right, and that gap is the one an adopter
// notices first, because correctness is the entire pitch.
//
// Refusal cases need no warehouse, so a pull request can assert that the
// model still declines a fan-out without any credential at all. Row cases
// need one, and a run without an executor reports how many it could not
// check rather than printing a clean pass over work it did not do.

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	common := addCommon(fs)
	path := fs.String("tests", "tests", "test file or a directory of them")
	asJSON := fs.Bool("json", false, "emit results as JSON for a CI step to read")
	failOnSkip := fs.Bool("fail-on-skip", false,
		"treat a case that could not run as a failure, which is what a pipeline"+
			" that believes it is checking rows should do")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	suites, err := modeltest.Load(*path)
	if err != nil {
		return err
	}

	ctx := context.Background()
	ex, closeExec, err := common.executor(ctx)
	if err != nil {
		return err
	}
	defer closeExec()

	// Strict: a namespace that will not load has to fail a test run. A server
	// isolates a broken namespace and keeps serving, which is right for a
	// server and wrong here, because the cases against it would report as
	// unknown metrics rather than as the load failure they are.
	eng, closeAudit, err := common.engine(ex, true, nil)
	if err != nil {
		return err
	}
	defer closeAudit()

	started := time.Now()
	results := modeltest.Run(ctx, eng, suites, common.id())
	summary := modeltest.Summarize(results)

	if *asJSON {
		if err := emitJSON(results, summary, time.Since(started)); err != nil {
			return err
		}
	} else {
		reportTests(results, summary, time.Since(started), eng.CanExecute())
	}

	if summary.Failed > 0 {
		return fmt.Errorf("%d test(s) failed", summary.Failed)
	}
	if *failOnSkip && summary.Skipped > 0 {
		return fmt.Errorf(
			"%d test(s) could not run and -fail-on-skip is set; configure a "+
				"warehouse or drop the flag", summary.Skipped)
	}
	return nil
}

func reportTests(results []modeltest.Result, s modeltest.Summary, took time.Duration, canExecute bool) {
	for _, r := range results {
		switch {
		case r.Skipped:
			fmt.Fprintf(os.Stderr, "  skip  %s\n        %s\n", r.Case.Name, r.Reason)
		case r.Passed:
			fmt.Fprintf(os.Stderr, "  ok    %s\n", r.Case.Name)
		default:
			fmt.Fprintf(os.Stderr, "  FAIL  %s\n        %s\n", r.Case.Name, r.Reason)
		}
	}

	fmt.Fprintf(os.Stderr, "\n%d passed, %d failed, %d skipped in %s\n",
		s.Passed, s.Failed, s.Skipped, took.Round(time.Millisecond))

	if s.Skipped > 0 && !canExecute {
		// Said plainly rather than left in the count. A green run that
		// checked no rows is the failure mode this whole command exists to
		// prevent somewhere else.
		fmt.Fprintf(os.Stderr,
			"\nNo warehouse is configured, so no row assertion ran. The refusal "+
				"cases above did run: they need no credentials, because the planner "+
				"declines before any SQL exists.\n")
	}
}

// emitJSON writes a machine-readable report.
//
// One object with a summary and every case, so a CI step can fail the build
// and annotate the pull request without parsing the human output.
func emitJSON(results []modeltest.Result, s modeltest.Summary, took time.Duration) error {
	type caseJSON struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Reason     string `json:"reason,omitempty"`
		DurationMS int64  `json:"duration_ms"`
	}
	out := struct {
		Passed     int        `json:"passed"`
		Failed     int        `json:"failed"`
		Skipped    int        `json:"skipped"`
		DurationMS int64      `json:"duration_ms"`
		Tests      []caseJSON `json:"tests"`
	}{
		Passed: s.Passed, Failed: s.Failed, Skipped: s.Skipped,
		DurationMS: took.Milliseconds(),
	}
	for _, r := range results {
		status := "failed"
		switch {
		case r.Skipped:
			status = "skipped"
		case r.Passed:
			status = "passed"
		}
		out.Tests = append(out.Tests, caseJSON{
			Name: r.Case.Name, Status: status, Reason: r.Reason,
			DurationMS: r.Duration.Milliseconds(),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
