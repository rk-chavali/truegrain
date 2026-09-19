package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/rk-chavali/truegrain/internal/engine"
)

// `truegrain doctor` asks the warehouse whether the model is still true.
//
// Everything else validates the model against itself: the YAML parses, the
// joins resolve, the expressions compile. None of that notices that somebody
// dropped a column last Tuesday. The first sign of that today is a caller
// getting an error, which is the most expensive place to find it.
//
// Metadata only. It reads no rows, needs no more than permission to list a
// schema, and is cheap enough to run on a schedule, which is the point: drift
// is found by looking regularly, not by looking once.

// exitCodeUnhealthy marks a model the warehouse contradicts, distinct from a
// usage error, a refusal and a crash so CI can tell them apart.
const exitCodeUnhealthy = 5

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	common := addCommon(fs)
	format := fs.String("format", "text", "output format: text or json")
	exitZero := fs.Bool("exit-zero", false, "report problems without failing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("unknown -format %q: use text or json", *format)
	}
	if err := common.resolve(); err != nil {
		return err
	}

	ctx := context.Background()
	ex, closeExec, err := common.executor(ctx)
	if err != nil {
		return err
	}
	defer closeExec()

	// Strict: doctor checks a model somebody is about to trust, so a namespace
	// that will not load is a finding rather than something to skip past.
	eng, closeAudit, err := common.engine(ex, true)
	if err != nil {
		return err
	}
	defer closeAudit()

	report, err := eng.Doctor(ctx)
	if err != nil {
		return err
	}

	if *format == "json" {
		if err := writeDiagnosisJSON(os.Stdout, report); err != nil {
			return err
		}
	} else {
		printDiagnosis(report)
	}

	if !report.OK() && !*exitZero {
		os.Exit(exitCodeUnhealthy)
	}
	return nil
}

// diagnosisJSON is the published shape. `ok` is first because it is the field
// a pipeline reads; everything else explains it.
type diagnosisJSON struct {
	OK       bool             `json:"ok"`
	Checked  int              `json:"tables_checked"`
	Findings []engine.Finding `json:"findings"`
	Skipped  string           `json:"skipped,omitempty"`
}

func writeDiagnosisJSON(out *os.File, d engine.Diagnosis) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(diagnosisJSON{
		OK:       d.OK(),
		Checked:  d.Checked,
		Findings: d.Findings,
		Skipped:  d.Skipped,
	})
}

func printDiagnosis(d engine.Diagnosis) {
	if d.Skipped != "" {
		fmt.Printf("Not checked: %s\n", d.Skipped)
		return
	}

	if len(d.Findings) == 0 {
		fmt.Printf("The warehouse agrees with the model. %d table(s) checked.\n", d.Checked)
		return
	}

	errors, warnings := 0, 0
	for _, f := range d.Findings {
		if f.Severity == "error" {
			errors++
		} else {
			warnings++
		}
	}

	fmt.Printf("%d table(s) checked.\n\n", d.Checked)
	for _, f := range d.Findings {
		marker := "!"
		if f.Severity == "warning" {
			marker = "?"
		}
		where := f.Dataset
		if f.Field != "" {
			where += "." + f.Field
		}
		fmt.Printf("  %s %s\n", marker, where)
		fmt.Printf("      %s\n", f.Message)
		if f.Source != "" {
			fmt.Printf("      in %s\n", f.Source)
		}
		if f.Hint != "" {
			fmt.Printf("      %s\n", f.Hint)
		}
		fmt.Println()
	}

	// The summary last, because it is what someone reads after scrolling.
	switch {
	case errors > 0 && warnings > 0:
		fmt.Printf("%d will break a query, %d will not break today.\n", errors, warnings)
	case errors > 0:
		fmt.Printf("%d will break a query.\n", errors)
	default:
		fmt.Printf("%d worth knowing, none of which breaks a query.\n", warnings)
	}
}
