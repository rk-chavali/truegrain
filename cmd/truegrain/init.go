package main

import (
	"bufio"
	"context"
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
		if *dataset == "" {
			*dataset = ask(in, "Dataset", "")
		}
		if err := introspect.ValidateDatasetID(*dataset); err != nil {
			return err
		}
		if *name == "" {
			*name = *dataset
		}
		fmt.Fprintf(os.Stderr, "\nReading %s.%s\n\n", *project, *dataset)
		schema, err = introspect.BigQuery(context.Background(), *project, *dataset)

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
			*dataset = ask(in, "Schema", "public")
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

	fmt.Fprintf(os.Stderr, `
Next:

  1. Write a metric in %s
  2. truegrain validate -models %s
  3. truegrain doctor   -models %s

Metrics are not generated, and that is deliberate: a schema can say an order
has a total, and only you can say which of those totals the business counts as
revenue.
`, filepath.Join(*dir, "models", *name, "metrics.yaml"), *dir, *dir)

	return nil
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
