package main

import (
	"flag"
	"fmt"
	"os"

	"strings"

	"github.com/rk-chavali/truegrain/internal/importer/cube"
	"github.com/rk-chavali/truegrain/internal/importer/dbt"
	"github.com/rk-chavali/truegrain/internal/importer/lookml"
	"github.com/rk-chavali/truegrain/internal/introspect"
)

// `truegrain import` carries an existing semantic layer across.
//
// `init` answers the cold start for somebody with a warehouse and no
// semantic layer. This answers it for somebody who already has one, which is
// the larger group and the harder sell: they have already argued about what
// revenue means and will not do it again to try another tool.
//
// It needs no warehouse credentials. A dbt manifest is produced by
// `dbt parse`, which connects to nothing, so this runs on a laptop and in a
// pull request.

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	from := fs.String("from", "dbt", "what to import from: dbt, lookml or cube")
	manifest := fs.String("manifest", "target/manifest.json",
		"path to the dbt manifest, produced by `dbt parse`")
	project := fs.String("project", ".",
		"path to the Looker or Cube project directory, for -from lookml or cube")
	dir := fs.String("dir", ".", "where to write the model repository")
	name := fs.String("namespace", "", "namespace for the imported model (default: the dbt project)")
	dialectName := fs.String("dialect", "", "warehouse dialect to configure (default: the project's adapter)")
	force := fs.Bool("force", false, "regenerate the generated model file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var schema introspect.Schema
	var skipped, notes []string
	var projectName string

	switch *from {
	case "dbt":
		s, report, err := dbt.Import(*manifest)
		if err != nil {
			return err
		}
		schema, skipped, notes = s, report.Skipped, report.Notes
		projectName = report.Project

	case "cube":
		s, report, err := cube.Import(*project)
		if err != nil {
			return err
		}
		schema, skipped, notes = s, report.Skipped, report.Notes
		projectName = report.Project

	case "lookml", "looker":
		s, report, err := lookml.Import(*project)
		if err != nil {
			return err
		}
		schema, skipped, notes = s, report.Skipped, report.Notes
		projectName = report.Project

	default:
		return fmt.Errorf(
			"import reads dbt, lookml and cube; %q is not read yet", *from)
	}

	if *name == "" {
		*name = sanitizeNamespace(projectName)
	}
	if *name == "" {
		*name = schema.Name
	}

	printImport(schema, projectName, skipped, notes)

	written, err := writeRepository(*dir, *name, defaultDialect(*dialectName), "", "", "", schema, *force)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	for _, f := range written {
		fmt.Fprintf(os.Stderr, "  wrote  %s\n", f)
	}

	fmt.Fprintf(os.Stderr, `
Next:

  1. truegrain validate -models %s
  2. truegrain test     -models %s   (write the assertions; see docs)
  3. truegrain diff     -models %s   against your next change

Read the metrics before trusting them. An importer carries across what the
source declared, and what this engine adds is that a question these cannot
answer correctly is now refused rather than answered with a number that
looks plausible.
`, *dir, *dir, *dir)
	return nil
}

// defaultDialect falls back to duckdb, which executes nothing until
// configured and is the honest placeholder: a model that names a warehouse
// it was not imported from would compile for the wrong one.
func defaultDialect(given string) string {
	if given != "" {
		return given
	}
	return "duckdb"
}

// sanitizeNamespace makes a dbt project name usable as a namespace.
func sanitizeNamespace(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-' || r == ' ' || r == '.':
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// printImport shows what came across and, more importantly, what did not.
//
// The skipped list is printed in full rather than counted. Somebody
// comparing two tools needs to know which of their metrics are missing
// before they compare any numbers, and a count does not tell them that.
func printImport(s introspect.Schema, project string, skipped, notes []string) {
	fmt.Fprintf(os.Stderr, "\nImported %s\n\n", project)

	for _, t := range s.Tables {
		key := strings.Join(t.PrimaryKey, ", ")
		if key == "" {
			key = "none"
		}
		fmt.Fprintf(os.Stderr, "  %-22s %2d fields   grain: %s\n", t.Name, len(t.Columns), key)
	}

	fmt.Fprintf(os.Stderr, "\n  %d metric(s)\n", len(s.Metrics))
	for _, m := range s.Metrics {
		fmt.Fprintf(os.Stderr, "    %-22s %s\n", m.Name, m.Expression)
	}

	fmt.Fprintf(os.Stderr, "\n  %d join(s), with the cardinality the source declared\n", len(s.Relationships))
	for _, rel := range s.Relationships {
		fmt.Fprintf(os.Stderr, "    %s.%s -> %s.%s\n",
			rel.From, strings.Join(rel.FromColumns, ","),
			rel.To, strings.Join(rel.ToColumns, ","))
	}

	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "\n  %d thing(s) NOT imported:\n", len(skipped))
		for _, x := range skipped {
			fmt.Fprintf(os.Stderr, "    - %s\n", x)
		}
	}
	if n := s.MetricsNeedingDescription(); n > 0 {
		fmt.Fprintf(os.Stderr,
			"\n  ! %d metric(s) arrived with no description and carry a TODO "+
				"placeholder. An agent reads the description to decide whether a "+
				"metric answers a question, so these are the first thing worth "+
				"writing: grep TODO in metrics.yaml\n", n)
	}
	for _, note := range append(notes, s.Notes...) {
		fmt.Fprintf(os.Stderr, "\n  ! %s\n", note)
	}
}
