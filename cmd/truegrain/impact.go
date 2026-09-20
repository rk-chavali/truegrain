package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/rk-chavali/truegrain/internal/lineage"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// What breaks if this changes.
//
// The question asked before every schema change, and until now the honest
// answer was to grep the model. Grep finds the string rather than the meaning:
// a field called `status` exists on four datasets and only one of them is the
// one being dropped.
//
// Needs no warehouse and no credential, because a model already declares
// everything the answer requires. That is what makes it a pull request check:
// it runs on a machine with access to nothing, in the same job as validate.
//
// It reports structure and stops there. A metric that reads a dropped field
// will not compile, which is a fact. Whether anybody cares about the dashboard
// built on that metric is not something a model can know, and guessing would
// make the output an opinion rather than a reference.

func cmdImpact(args []string) error {
	fs := flag.NewFlagSet("impact", flag.ExitOnError)
	models := fs.String("models", ".", "workspace root, model directory, or a single Ossie file")
	field := fs.String("field", "", "a `dataset.field` to report on")
	dataset := fs.String("dataset", "", "a dataset to report on, as though the whole table were going away")
	metric := fs.String("metric", "", "a metric to report on, in the other direction: what it reads")
	format := fs.String("format", "text", "output format: text or json")
	strict := fs.Bool("strict", false,
		"exit non-zero when anything depends on the subject, so a pull request fails until somebody has looked")
	if err := fs.Parse(args); err != nil {
		return err
	}

	asked := 0
	for _, s := range []string{*field, *dataset, *metric} {
		if s != "" {
			asked++
		}
	}
	if asked != 1 {
		return fmt.Errorf("name exactly one of -field, -dataset or -metric. " +
			"For example: truegrain impact -field orders.order_total")
	}

	ws, err := workspace.Load(*models, workspace.Options{})
	if err != nil {
		return err
	}

	var found []lineage.Impact
	for _, ns := range ws.Namespaces() {
		if ns.Model == nil {
			continue
		}
		g := lineage.Build(ns.Name, ns.Model)
		var got lineage.Impact
		switch {
		case *field != "":
			got = g.Field(strip(ns.Name, *field))
		case *dataset != "":
			got = g.Dataset(strip(ns.Name, *dataset))
		default:
			got = g.Metric(strip(ns.Name, *metric))
		}
		// Reported from every namespace that has it. A field name can exist in
		// several, and answering from only the first would be the same mistake
		// as grepping.
		if got.Found {
			found = append(found, got)
		}
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(found); err != nil {
			return err
		}
	} else {
		writeImpact(found, subjectOf(*field, *dataset, *metric))
	}

	if len(found) == 0 {
		// Nothing found is not nothing depends on it. A typo produces an empty
		// answer that reads exactly like a safe change, so it exits non-zero
		// whether or not -strict was given.
		return fmt.Errorf("%s is not declared in this workspace, so nothing was checked",
			subjectOf(*field, *dataset, *metric))
	}
	if *strict {
		for _, i := range found {
			if i.Breaks() {
				return fmt.Errorf("something depends on %s; see above", i.Subject)
			}
		}
	}
	return nil
}

func subjectOf(field, dataset, metric string) string {
	for _, s := range []string{field, dataset, metric} {
		if s != "" {
			return s
		}
	}
	return ""
}

// strip removes a namespace prefix somebody typed, so both `orders.total` and
// `retail.orders.total` reach the same field.
func strip(namespace, name string) string {
	return strings.TrimPrefix(name, namespace+".")
}

func writeImpact(found []lineage.Impact, subject string) {
	if len(found) == 0 {
		fmt.Printf("%s is not declared in this workspace.\n", subject)
		return
	}
	for _, i := range found {
		fmt.Printf("\n%s\n", i.Subject)

		if i.Kind == "metric" {
			list("reads", i.Fields)
			list("through joins", i.Relationships)
			continue
		}

		if !i.Breaks() {
			fmt.Println("  nothing in the model refers to this.")
			if i.Kind == "dataset" && len(i.Fields) > 0 {
				fmt.Printf("  it declares %d field(s), none of them referenced.\n", len(i.Fields))
			}
		} else {
			// Ordered by how badly each one fails. A metric stops compiling
			// and somebody notices; a lost grain turns sums into refusals,
			// which is quieter and worse.
			list("metrics that read it, which stop compiling", i.Metrics)
			list("joins keyed on it, which disconnect", i.Relationships)
			list("the declared grain of, which loses fan-out detection", i.PrimaryKeyOf)
		}

		// Printed either way. A groupable field nothing in the model
		// references still has callers grouping by it, and the model cannot
		// know whether anybody is. Saying nothing here reads as permission.
		if i.Dimension {
			fmt.Println("  groupable: somebody may be grouping by this. The model cannot say.")
		}
	}
	fmt.Println()
}

func list(label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("  %s:\n", label)
	for _, s := range items {
		fmt.Printf("    %s\n", s)
	}
}
