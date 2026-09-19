package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/modeldiff"
)

// `truegrain diff` answers one question: does this change alter a number?
//
// docs/08-contributing.md calls an undocumented change to a compiled result a
// silent correctness incident, which is exactly right: the model still
// validates, the tests still pass, and every dashboard quietly moves. Reviewing
// a YAML diff does not catch it, because the YAML change is usually small and
// the consequence is not.
//
// So the comparison is made on compiled SQL rather than on the model text. Two
// workspaces are each compiled across the same deterministic matrix of requests
// and the results are compared. Renaming a description is invisible here;
// changing a join, a grain or an expression is not.

// exitCodeChanged marks "compiled output differs", distinct from a refusal and
// from a crash so CI can tell them apart.
const exitCodeChanged = 4

// dimensionsPerMetric bounds the matrix. Every metric is compiled alone and
// then grouped by a few of its dimensions, which is enough to catch a changed
// join path without compiling a combinatorial explosion on a large model.
const dimensionsPerMetric = 3

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	common := addCommon(fs)
	against := fs.String("against", "",
		"workspace root to compare against, usually a checkout of the base branch")
	full := fs.Bool("full", false, "print the SQL of every change, not just a summary")
	exitZero := fs.Bool("exit-zero", false, "report differences without failing")
	format := fs.String("format", "text", "output format: text, json or markdown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" && *format != "markdown" {
		return fmt.Errorf("unknown -format %q: use text, json or markdown", *format)
	}
	if err := common.resolve(); err != nil {
		return err
	}
	if *against == "" {
		return fmt.Errorf("diff needs -against pointing at another workspace root\n" +
			"  in CI: git worktree add /tmp/base origin/main && semantic diff -against /tmp/base")
	}

	current, err := snapshot(*common.models, common.discover(), *common.dialect)
	if err != nil {
		return fmt.Errorf("compiling the current workspace:\n%w", err)
	}
	baseline, err := snapshot(*against, common.discover(), *common.dialect)
	if err != nil {
		return fmt.Errorf("compiling the baseline at %s:\n%w", *against, err)
	}

	report := modeldiff.Compare(baseline, current)
	switch *format {
	case "json":
		if err := reportWriteJSON(report, os.Stdout, *full); err != nil {
			return err
		}
	case "markdown":
		reportWriteMarkdown(report, os.Stdout, *full)
	default:
		reportPrint(report, os.Stdout, *full)
	}

	if report.Changed() && !*exitZero {
		os.Exit(exitCodeChanged)
	}
	return nil
}

// snapshot loads a workspace and compiles the matrix against it.
//
// Access control is deliberately not applied: this compares what the model
// means, and a policy difference between two checkouts would otherwise show up
// as a metric vanishing.
func snapshot(models string, discover []string, dialectName string) (modeldiff.Snapshot, error) {
	eng, err := engine.New(engine.Config{
		ModelPath: models,
		Discover:  discover,
		Strict:    true,
		Dialect:   dialectName,
		Resolver:  govern.AllowAll{},
	})
	if err != nil {
		return nil, err
	}
	return modeldiff.Take(context.Background(), eng)
}

func reportPrint(r modeldiff.Report, out *os.File, full bool) {
	if !r.Changed() {
		fmt.Fprintln(out, "No compiled result changed.")
		return
	}

	// Removals and alterations come first: they are the ones that move a number
	// somebody is already looking at.
	if len(r.Removed) > 0 {
		fmt.Fprintf(out, "Removed (%d). Anything asking for these now fails:\n", len(r.Removed))
		for _, label := range r.Removed {
			fmt.Fprintf(out, "  - %s\n", label)
		}
		fmt.Fprintln(out)
	}

	if len(r.Altered) > 0 {
		labels := make([]string, 0, len(r.Altered))
		for label := range r.Altered {
			labels = append(labels, label)
		}
		sort.Strings(labels)

		fmt.Fprintf(out, "Changed meaning (%d). These compile differently than before:\n", len(labels))
		for _, label := range labels {
			pair := r.Altered[label]
			fmt.Fprintf(out, "  ~ %s\n", label)
			if full {
				fmt.Fprintf(out, "%s\n", indentBlock("      before: ", pair.Before))
				fmt.Fprintf(out, "%s\n", indentBlock("      after:  ", pair.After))
			}
		}
		fmt.Fprintln(out)
	}

	if len(r.Added) > 0 {
		fmt.Fprintf(out, "Added (%d):\n", len(r.Added))
		for _, label := range r.Added {
			fmt.Fprintf(out, "  + %s\n", label)
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out,
		"A change to the compiled output of an unchanged model is a major version bump,\n"+
			"even when the API is untouched. If this is intended, say so in the pull request.")
	if !full {
		fmt.Fprintln(out, "Re-run with -full to see the SQL on both sides.")
	}
}

func indentBlock(prefix, body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = prefix + line
			continue
		}
		lines[i] = strings.Repeat(" ", len(prefix)) + line
	}
	return strings.Join(lines, "\n")
}
