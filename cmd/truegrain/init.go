package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rk-chavali/truegrain/internal/config"
	"github.com/rk-chavali/truegrain/internal/introspect"
)

// `truegrain init` turns a warehouse into a model you can edit.
//
// The honest answer to "how do I try this on my data" was to hand-write Ossie
// YAML, which is the cold start every semantic layer hits and the reason most
// of them are opened once. This reads what the warehouse already knows and
// writes the parts that are derivable, leaving the parts that are not.
//
// It is a human command, run once. Nothing in a pipeline runs it: CI runs
// validate and diff, which need no warehouse at all, and doctor, which needs
// the same metadata-only credential this does. README.md, "Which commands need
// warehouse credentials", is the table.
//
// Credentials are never asked for. They come from the environment the same way
// they do everywhere else in this binary, so the command works unchanged on a
// laptop with gcloud, in a container with a mounted service account, and on a
// runner using workload identity federation.

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "where to write the model repository")
	dialect := fs.String("dialect", "", "warehouse: bigquery or postgres (asked for if omitted)")
	project := fs.String("project", "", "BigQuery project")
	dataset := fs.String("dataset", "", "dataset or schema to read")
	location := fs.String("location", "", "BigQuery location, for example US or europe-west2")
	dsnEnv := fs.String("dsn-env", "", "environment variable holding the PostgreSQL"+
		" connection string. Named rather than passed, because a connection string on"+
		" the command line lands in shell history and in the process list")
	name := fs.String("namespace", "", "namespace for the generated model (default: the dataset)")
	force := fs.Bool("force", false, "regenerate the generated model file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	if *dialect == "" {
		chosen, err := chooseWarehouse(in)
		if err != nil {
			return err
		}
		*dialect = chosen
	}

	// Said before anything is asked for, because "Dataset:" on its own reads
	// like the model is about to be stored there. It is not: nothing in this
	// binary ever writes to the warehouse. One dataset is where the first
	// draft comes from, and the model can name tables anywhere afterwards.
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "I read what the warehouse already knows and write you a first draft.")
	fmt.Fprintln(os.Stderr, "The model is YAML on your disk, in your repository: nothing is ever")
	fmt.Fprintln(os.Stderr, "stored in the warehouse, and you edit it from there.")
	fmt.Fprintln(os.Stderr)

	var schema introspect.Schema
	var err error

	switch *dialect {
	case "bigquery":
		if *project == "" {
			*project = ask(in, "BigQuery project", os.Getenv("GOOGLE_CLOUD_PROJECT"))
		}
		if *project == "" {
			return fmt.Errorf("a BigQuery project is required")
		}
		chosen := []string{}
		if *dataset != "" {
			chosen = []string{*dataset}
		} else {
			chosen, err = chooseDatasets(in, *project)
			if err != nil {
				return err
			}
		}
		for _, d := range chosen {
			if err := introspect.ValidateDatasetID(d); err != nil {
				return err
			}
		}
		// A namespace per dataset, which is what models/<namespace>/ already
		// expects and what keeps ownership answerable: one team owns one
		// dataset's definitions, and -namespace renaming that only makes
		// sense when there is one of them.
		if *name != "" && len(chosen) > 1 {
			return fmt.Errorf("-namespace names one namespace, and %d datasets were chosen. "+
				"Each dataset becomes its own namespace, so run init once per dataset to "+
				"name them", len(chosen))
		}
		return initDatasets(*dir, *project, *location, chosen, *name, *force)

	case "postgres":
		if *dsnEnv == "" {
			*dsnEnv = ask(in, "Environment variable holding the connection string", "DATABASE_URL")
		}
		// The variable is named, and only then read. A connection string passed
		// as a flag is visible in shell history and to anything that can list
		// processes, and a password is the one value that must be in neither.
		dsn := os.Getenv(*dsnEnv)
		if dsn == "" {
			return fmt.Errorf(
				"-dsn-env names %s, which is not set. Export the PostgreSQL connection "+
					"string in it, for example\n"+
					"  export %s=postgres://reader@localhost:5432/warehouse?sslmode=require",
				*dsnEnv, *dsnEnv)
		}
		if *dataset == "" {
			*dataset = ask(in, "Schema to read tables from", "public")
		}
		if err := introspect.ValidateSchemaName(*dataset); err != nil {
			return err
		}
		if *name == "" {
			*name = *dataset
		}
		fmt.Fprintf(os.Stderr, "\nReading schema %s\n\n", *dataset)
		schema, err = introspect.Postgres(context.Background(), dsn, *dataset)

	default:
		return fmt.Errorf("init reads bigquery and postgres; %q is not read yet. "+
			"Other warehouses are supported for compiling and querying, so a model "+
			"written by hand or generated elsewhere still works", *dialect)
	}
	if err != nil {
		return err
	}
	printSchema(schema)

	written, err := writeRepository(*dir, *name, *dialect, *project, *location, *dsnEnv, schema, *force)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr)
	for _, f := range written {
		fmt.Fprintf(os.Stderr, "  wrote  %s\n", f)
	}

	reportNextSteps(*dir, *name)
	return nil
}

// reportNextSteps says what is left, which is the half no schema can write.
func reportNextSteps(dir, namespace string) {
	fmt.Fprintf(os.Stderr, `
Next:

  1. Write a metric in %s
  2. truegrain validate -models %s
  3. truegrain doctor   -models %s

Metrics are not generated, and that is deliberate: a schema can say an order
has a total, and only you can say which of those totals the business counts as
revenue.
`, filepath.Join(dir, "models", namespace, "metrics.yaml"), dir, dir)
}

// chooseDatasets offers what the credential can see.
//
// Numbered, like the warehouse question above, because typing a dataset name
// means already knowing it and the person running this is often exactly the
// one who does not. A table count is shown beside each: "which of these has my
// data in it" is the real question, and a count answers it faster than a name.
func chooseDatasets(in *bufio.Reader, project string) ([]string, error) {
	fmt.Fprintf(os.Stderr, "Reading the datasets in %s\n\n", project)
	summaries, err := introspect.BigQueryDatasets(context.Background(), project)
	if err != nil {
		return nil, err
	}
	if len(summaries) == 0 {
		return nil, fmt.Errorf("%s has no datasets this credential can see. "+
			"Check the project, or ask for bigquery.datasets.get on one", project)
	}

	total := 0
	for _, d := range summaries {
		total += d.Tables
	}

	fmt.Fprintln(os.Stderr, "Which dataset should I read?")
	fmt.Fprintln(os.Stderr)
	for i, d := range summaries {
		count := fmt.Sprintf("%d tables", d.Tables)
		if d.Unreadable {
			// Listed rather than hidden: a dataset missing from the menu
			// looks like it does not exist, which sends somebody to the
			// wrong place entirely.
			count = "tables not readable with this credential"
		} else if d.Tables == 1 {
			count = "1 table"
		}
		fmt.Fprintf(os.Stderr, "  %-5s %-28s %s\n", label(i+1), d.Name, count)
	}
	fmt.Fprintf(os.Stderr, "  %-5s %-28s %d tables, one namespace each\n",
		label(len(summaries)+1), "every dataset", total)
	fmt.Fprintln(os.Stderr)

	answer := ask(in, "Enter a number, a name, or several numbers separated by commas", "1")
	return resolveDatasets(answer, summaries)
}

// label renders the number somebody types, padded as one unit.
//
// Padding the name alone lost the columns at ten datasets, because the bracket
// grows and the name's column does not move with it.
func label(n int) string {
	return "[" + strconv.Itoa(n) + "]"
}

// resolveDatasets turns what was typed into dataset names.
//
// Separate from the prompt so the parsing is testable without a terminal,
// which is the half that can silently read "1,3" as one dataset called "1,3".
func resolveDatasets(answer string, summaries []introspect.DatasetSummary) ([]string, error) {
	names := make([]string, 0, len(summaries))
	for _, d := range summaries {
		names = append(names, d.Name)
	}

	answer = strings.TrimSpace(answer)
	if strings.EqualFold(answer, "all") || answer == strconv.Itoa(len(summaries)+1) {
		return names, nil
	}

	var chosen []string
	seen := map[string]bool{}
	for _, part := range strings.Split(answer, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := ""
		if n, err := strconv.Atoi(part); err == nil {
			if n < 1 || n > len(summaries) {
				return nil, fmt.Errorf("%d is not one of the %d datasets listed", n, len(summaries))
			}
			name = summaries[n-1].Name
		} else {
			for _, d := range summaries {
				if strings.EqualFold(d.Name, part) {
					name = d.Name
					break
				}
			}
			if name == "" {
				return nil, fmt.Errorf("%q is not a dataset in this project", part)
			}
		}
		if !seen[name] {
			seen[name] = true
			chosen = append(chosen, name)
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("no dataset chosen")
	}
	return chosen, nil
}

// initDatasets reads each chosen dataset and writes it as its own namespace.
//
// One dataset that fails to read stops the run rather than leaving a repository
// half written: a model missing a namespace nobody noticed is worse than a
// command that says which dataset it could not read.
func initDatasets(dir, project, location string, datasets []string, namespace string, force bool) error {
	var written []string
	for _, d := range datasets {
		name := namespace
		if name == "" {
			name = d
		}
		fmt.Fprintf(os.Stderr, "\nReading %s.%s\n\n", project, d)
		schema, err := introspect.BigQuery(context.Background(), project, d)
		if errors.Is(err, introspect.ErrNoTables) && len(datasets) > 1 {
			// Reported and stepped over. Somebody who asked to read everything
			// should not have the run die on an empty dataset after writing
			// three namespaces, which leaves a repository half made.
			fmt.Fprintln(os.Stderr, "  skipped: nothing to model here")
			continue
		}
		if err != nil {
			return err
		}
		printSchema(schema)

		files, err := writeRepository(dir, name, "bigquery", project, location, "", schema, force)
		if err != nil {
			return err
		}
		written = append(written, files...)
	}

	fmt.Fprintln(os.Stderr)
	for _, f := range written {
		fmt.Fprintf(os.Stderr, "  wrote  %s\n", f)
	}
	reportNextSteps(dir, namespaceOf(datasets, namespace))
	return nil
}

// namespaceOf names the one to point somebody at in the next steps.
func namespaceOf(datasets []string, namespace string) string {
	if namespace != "" {
		return namespace
	}
	return datasets[0]
}

// printSchema shows what was found, because seeing the keys is how somebody
// knows whether the model is going to be any good.
func printSchema(s introspect.Schema) {
	for _, t := range s.Tables {
		key := strings.Join(t.PrimaryKey, ", ")
		if key == "" {
			key = "none"
		}
		fmt.Fprintf(os.Stderr, "  %-20s %2d columns   key: %s\n", t.Name, len(t.Columns), key)
	}

	fmt.Fprintf(os.Stderr, "\n  %d relationship(s) from declared foreign keys\n", len(s.Relationships))
	for _, r := range s.Relationships {
		fmt.Fprintf(os.Stderr, "    %s.%s -> %s.%s\n",
			r.From, strings.Join(r.FromColumns, ","), r.To, strings.Join(r.ToColumns, ","))
	}

	for _, note := range s.Notes {
		fmt.Fprintf(os.Stderr, "\n  ! %s\n", note)
	}
}

// writeRepository puts the model repository on disk, refusing to clobber
// anything it did not generate.
//
// The datasets file is the only one worth regenerating, so it is the only one
// -force will replace. Overwriting somebody's metrics because they re-ran a
// setup command would be unforgivable, and a namespace's owners are a decision
// no schema can reproduce.
func writeRepository(dir, namespace, dialect, project, location, dsnEnv string,
	s introspect.Schema, force bool) ([]string, error) {

	models := filepath.Join(dir, "models", namespace)
	if err := os.MkdirAll(models, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", models, err)
	}

	files := []struct {
		path        string
		contents    string
		regenerable bool
	}{
		{filepath.Join(dir, config.FileName), projectFile(dialect, project, location, dsnEnv), false},
		{filepath.Join(models, "namespace.yaml"), s.Namespace(namespace), false},
		{filepath.Join(models, "datasets.generated.yaml"), s.Datasets(namespace), true},
		{filepath.Join(models, "metrics.yaml"), s.MetricsFile(namespace), false},
	}

	var written []string
	for _, f := range files {
		if _, err := os.Stat(f.path); err == nil {
			if !f.regenerable {
				fmt.Fprintf(os.Stderr, "  kept   %s (already exists)\n", f.path)
				continue
			}
			if !force {
				return nil, fmt.Errorf(
					"%s already exists; re-run with -force to regenerate it.\n"+
						"That is safe: it is the generated half, and your metrics are never touched",
					f.path)
			}
		}
		if err := os.WriteFile(f.path, []byte(f.contents), 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", f.path, err)
		}
		written = append(written, f.path)
	}
	return written, nil
}

// projectFile renders truegrain.yaml, so the flags stop having to be repeated.
func projectFile(dialect, project, location, dsnEnv string) string {
	var b strings.Builder

	b.WriteString(`# Settings for this model repository, found by walking up from -models.
#
# Any flag given on the command line wins, so a developer can point a
# configured repository at a scratch dataset without editing this.
#
# There is deliberately nowhere here to put a credential. They come from the
# environment, because a key file path in a committed file becomes a key file
# in a git repository. Values that must stay out of version control are written
# as ${VAR} and read from the environment.

version: 1

warehouse:
`)
	fmt.Fprintf(&b, "  dialect: %s\n", dialect)

	switch dialect {
	case "postgres":
		// The variable name, not the connection string. Writing the DSN here
		// would put a password in a file people commit, which is the failure
		// this whole arrangement exists to prevent.
		fmt.Fprintf(&b, "  dsn_env: %s\n", dsnEnv)
		b.WriteString(`  #   The name of the variable, never the connection string itself. This
  #   file is committed, and a password in git is a password to rotate.
  # impersonate: true
  #   Run each query as the calling identity with SET LOCAL ROLE, so the
  #   server's own row-level security applies to them rather than to the
  #   connecting user. Needs a database role per caller, and refuses every
  #   query where one does not exist.
`)
	default:
		fmt.Fprintf(&b, "  project: %s\n", project)
		if location != "" {
			fmt.Fprintf(&b, "  location: %s\n", location)
		} else {
			b.WriteString("  # location: US        # set this if BigQuery cannot infer the region\n")
		}
		b.WriteString(`  # max_bytes_billed: 1000000000
  #   Refuse a query the warehouse estimates will cost more than this, before
  #   it runs. Without it a single bad question can be expensive.
  # impersonate: true
  #   Run each query as the calling user rather than as this process, so the
  #   warehouse's own row and column security applies to them and not to the
  #   engine's service account.
`)
	}

	b.WriteString(`
# governance:
#   policy: policy.yaml
#     Which columns each caller may read, and which rows. Without this file
#     nothing is enforced, and "truegrain health" says so.

# audit:
#   sink: file
#   path: audit.jsonl
#     Every decision, including every refusal, with no values in it.
`)
	return b.String()
}

// warehouse is one entry on the menu `init` opens with.
//
// `why` is empty when init can read this warehouse, and says why it cannot
// when it is not. A warehouse the engine can query but not yet read is a
// confusing thing to meet halfway through a setup, so it is named up front.
type warehouse struct {
	name  string
	label string
	why   string
}

// warehouses is the menu, in the order it is offered.
//
// Deliberately not derived from dialect.Names(). That list is every dialect the
// engine can compile for, which is a different and much longer list than the
// ones `init` can read, and offering a choice that then refuses is worse than
// not offering it.
var warehouses = []warehouse{
	{name: "bigquery", label: "BigQuery"},
	{name: "postgres", label: "PostgreSQL"},
	{name: "snowflake", label: "Snowflake", why: "runs queries, but its schema reader is not built yet"},
}

// chooseWarehouse asks which warehouse to read, the way gcloud asks.
//
// A numbered list rather than a free-text prompt with a default, because the
// free-text version was undiscoverable: it never said what was on offer, and a
// typo produced a refusal rather than a second chance.
//
// Both forms are accepted. `1` is what somebody sitting at a terminal types,
// and `bigquery` is what somebody who has done this before types, or what they
// copied out of a runbook. Refusing either would be a small cruelty.
func chooseWarehouse(in *bufio.Reader) (string, error) {
	readable := make([]warehouse, 0, len(warehouses))
	for _, w := range warehouses {
		if w.why == "" {
			readable = append(readable, w)
		}
	}

	fmt.Fprintln(os.Stderr, "Which warehouse should I read?")
	fmt.Fprintln(os.Stderr)
	for i, w := range readable {
		fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, w.label)
	}
	for _, w := range warehouses {
		if w.why != "" {
			// Listed without a number. Offering a choice that cannot be taken
			// wastes somebody's next thirty seconds; saying plainly that it is
			// not ready costs them nothing.
			fmt.Fprintf(os.Stderr, "      %-11s %s\n", w.label, w.why)
		}
	}
	fmt.Fprintln(os.Stderr)

	for {
		fmt.Fprintf(os.Stderr, "Enter a number or a name [%s]: ", readable[0].name)
		line, err := in.ReadString('\n')
		answer := strings.TrimSpace(line)

		if answer == "" {
			if err != nil {
				// Nothing left to read: stdin is a pipe, a closed terminal, or
				// a CI runner. Asking again would spin forever, which is how a
				// prompt-driven command hangs a pipeline until it is killed.
				return "", fmt.Errorf(
					"no warehouse given, and there is nothing to read an answer from.\n" +
						"Pass -dialect when this runs unattended, for example\n" +
						"  truegrain init -dialect bigquery -project my-project -dataset main")
			}
			return readable[0].name, nil
		}

		if choice, convErr := strconv.Atoi(answer); convErr == nil {
			if choice >= 1 && choice <= len(readable) {
				return readable[choice-1].name, nil
			}
			fmt.Fprintf(os.Stderr, "  There is no option %d.\n", choice)
			continue
		}

		for _, w := range warehouses {
			if strings.EqualFold(answer, w.name) || strings.EqualFold(answer, w.label) {
				if w.why != "" {
					return "", fmt.Errorf("init cannot read %s: %s", w.label, w.why)
				}
				return w.name, nil
			}
		}
		fmt.Fprintf(os.Stderr, "  %q is not a warehouse this reads.\n", answer)
	}
}

// ask prompts, offering a default the caller can accept with Enter.
func ask(in *bufio.Reader, question, fallback string) string {
	if fallback != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", question, fallback)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", question)
	}
	line, err := in.ReadString('\n')
	if answer := strings.TrimSpace(line); answer != "" {
		return answer
	}
	if err != nil {
		return fallback
	}
	return fallback
}
