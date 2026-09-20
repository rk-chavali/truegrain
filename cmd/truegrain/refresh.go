package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/config"
	"github.com/rk-chavali/truegrain/internal/introspect"
)

// Re-reading the warehouse after the warehouse moved.
//
// init is a command somebody runs once, and a schema keeps changing after
// that. Until now the only way to pick up a dropped column was to run init
// again with -force, which overwrites the generated file and shows nothing:
// you learn what moved by reading a diff afterwards, if you thought to look.
//
// This is the other half of doctor. doctor says the model and the warehouse
// disagree and stops there, because deciding what to do about a dropped column
// is a person's job. refresh is what that person runs next.
//
// It only ever touches the generated file. Metrics, the relationships somebody
// wrote by hand and a namespace's owners are never regenerated, for the reason
// init gives: overwriting them because a column moved would be unforgivable.
//
// Without -write it changes nothing and exits non-zero when the warehouse has
// moved, which is what makes it a CI stage. Run it on a schedule and open a
// pull request when it fails, and schema drift becomes a review rather than an
// incident.

func cmdRefresh(args []string) error {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	dir := fs.String("models", ".", "model repository to refresh")
	configPath := fs.String("config", "", "project file; default is the nearest truegrain.yaml at or above -models")
	write := fs.Bool("write", false, "apply the changes rather than only reporting them")
	only := fs.String("namespace", "", "refresh one namespace rather than every one")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *configPath
	if path == "" {
		found, ok := config.Discover(*dir)
		if !ok {
			return fmt.Errorf("no %s at or above %s. refresh re-reads the warehouse "+
				"the project file names, so it needs one", config.FileName, *dir)
		}
		path = found
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if cfg.Warehouse.Dialect != "bigquery" {
		return fmt.Errorf(
			"refresh reads bigquery, and this repository is configured for %q. "+
				"The other warehouses compile and query; only their readers are missing",
			cfg.Warehouse.Dialect)
	}
	if cfg.Warehouse.Project == "" {
		return fmt.Errorf("truegrain.yaml names no BigQuery project to read")
	}

	found, err := generatedFiles(*dir, *only)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("no generated model files under %s. "+
			"refresh updates what init wrote; run init first", filepath.Join(*dir, "models"))
	}

	moved := false
	for _, g := range found {
		changed, err := refreshOne(cfg.Warehouse.Project, g, *write)
		if err != nil {
			return err
		}
		moved = moved || changed
	}

	fmt.Fprintln(os.Stderr)
	switch {
	case !moved:
		fmt.Fprintln(os.Stderr, "Every model matches the warehouse.")
	case *write:
		fmt.Fprintln(os.Stderr, "Written. Commit the generated files and open a pull request:")
		fmt.Fprintln(os.Stderr, "the diff is the record of what the warehouse did.")
	default:
		// Non-zero so a scheduled run is a failing check rather than a line in
		// a log nobody reads. The report above already said what moved, so
		// this says only what to do about it.
		return fmt.Errorf("the warehouse has moved and nothing was written. " +
			"Re-run with -write to apply it, then validate to see whether " +
			"anything downstream broke")
	}
	return nil
}

// generated is one namespace's generated file and the dataset behind it.
type generated struct {
	namespace string
	dataset   string
	path      string
}

// generatedFiles finds what init wrote.
//
// The dataset is read from the file rather than assumed from the directory
// name, because -namespace lets the two differ and refreshing the wrong
// dataset into a namespace would be silent and very hard to see.
func generatedFiles(dir, only string) ([]generated, error) {
	root := filepath.Join(dir, "models")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", root, err)
	}

	var out []generated
	for _, e := range entries {
		if !e.IsDir() || (only != "" && e.Name() != only) {
			continue
		}
		path := filepath.Join(root, e.Name(), "datasets.generated.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		dataset := datasetOf(string(raw))
		if dataset == "" {
			return nil, fmt.Errorf("%s does not say which dataset it came from, "+
				"so refresh cannot know what to re-read. Re-run init for this namespace", path)
		}
		out = append(out, generated{namespace: e.Name(), dataset: dataset, path: path})
	}
	if only != "" && len(out) == 0 {
		return nil, fmt.Errorf("no namespace called %q under %s", only, root)
	}
	return out, nil
}

// datasetOf reads the dataset name out of a source line.
//
// The header comment names it too, but a comment is prose and a source is the
// thing the engine actually reads, so they cannot drift apart.
func datasetOf(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		rest, ok := strings.CutPrefix(line, "source:")
		if !ok {
			continue
		}
		qualified := strings.TrimSpace(rest)
		if dataset, _, found := strings.Cut(qualified, "."); found {
			return dataset
		}
	}
	return ""
}

func refreshOne(project string, g generated, write bool) (bool, error) {
	fmt.Fprintf(os.Stderr, "\n%s  (%s.%s)\n", g.namespace, project, g.dataset)

	schema, err := introspect.BigQuery(context.Background(), project, g.dataset)
	if err != nil {
		return false, err
	}
	fresh := schema.Datasets(g.namespace)

	current, err := os.ReadFile(g.path)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", g.path, err)
	}
	// Compared with line endings normalised, so a repository checked out on
	// Windows does not report every namespace as changed on every run.
	if normalise(string(current)) == normalise(fresh) {
		fmt.Fprintln(os.Stderr, "  unchanged")
		return false, nil
	}

	reportDrift(string(current), fresh)

	if write {
		if err := os.WriteFile(g.path, []byte(fresh), 0o644); err != nil {
			return true, fmt.Errorf("writing %s: %w", g.path, err)
		}
		fmt.Fprintf(os.Stderr, "  wrote %s\n", g.path)
	}
	return true, nil
}

func normalise(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// reportDrift says what moved, in model terms.
//
// Both sides are rendered by the same function, so scanning them with the same
// reader cannot disagree about the shape: the only thing that differs is what
// the warehouse said. A table or column that arrives or leaves is named, and a
// change too fine to name is reported as such rather than guessed at, because
// the committed diff is the exact record and this is only the summary.
func reportDrift(current, fresh string) {
	was, now := tablesIn(current), tablesIn(fresh)

	for _, name := range sortedKeys(now) {
		before, existed := was[name]
		if !existed {
			fmt.Fprintf(os.Stderr, "  + table %s (%d columns)\n", name, len(now[name].columns))
			continue
		}
		// Unchanged tables say nothing. Reporting every one of them as moved
		// because some other table did made the report useless on a dataset
		// of any size, which is every dataset that matters.
		if before.body == now[name].body {
			continue
		}
		added := missing(now[name].columns, before.columns)
		removed := missing(before.columns, now[name].columns)
		if len(added) > 0 {
			fmt.Fprintf(os.Stderr, "  + %s: %s\n", name, strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			// The one that breaks a query, so it must be unmissable.
			fmt.Fprintf(os.Stderr, "  - %s: %s\n", name, strings.Join(removed, ", "))
		}
		if len(added) == 0 && len(removed) == 0 {
			fmt.Fprintf(os.Stderr, "  ~ %s: a type, a key or a description moved\n", name)
		}
	}
	for _, name := range sortedKeys(was) {
		if _, still := now[name]; !still {
			fmt.Fprintf(os.Stderr, "  - table %s\n", name)
		}
	}
}

// table is one dataset as the generated file renders it.
type table struct {
	columns []string
	// body is the block verbatim, which is what says a type or a key moved
	// when the column names did not.
	body string
}

// tablesIn reads the datasets out of a generated file.
//
// Indentation is the discriminator, which is safe only because this reads what
// Schema.Datasets wrote: a dataset is six spaces deep and a field is ten.
//
// Scanning stops at the relationships block, which sits at the same depth as
// datasets and whose entries are also `- name:`. Without that the joins were
// reported as tables, so adding one table with a foreign key looked like two
// tables arriving and one of them had no columns.
func tablesIn(body string) map[string]table {
	out := map[string]table{}
	name := ""
	var block []string

	flush := func() {
		if name != "" {
			t := out[name]
			t.body = strings.Join(block, "\n")
			out[name] = t
		}
		name, block = "", nil
	}

	for _, line := range strings.Split(normalise(body), "\n") {
		// Any key at four spaces ends the datasets list: relationships, or
		// anything a later version of the renderer adds beside it.
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") {
			flush()
			if strings.TrimSpace(line) != "datasets:" {
				break
			}
			continue
		}
		if found, ok := strings.CutPrefix(line, "      - name: "); ok {
			flush()
			name = strings.TrimSpace(found)
			out[name] = table{}
		}
		if name == "" {
			continue
		}
		block = append(block, line)
		if column, ok := strings.CutPrefix(line, "          - name: "); ok {
			t := out[name]
			t.columns = append(t.columns, strings.TrimSpace(column))
			out[name] = t
		}
	}
	flush()
	return out
}

func sortedKeys(m map[string]table) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// missing returns what is in a and not in b.
func missing(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, s := range b {
		have[s] = true
	}
	var out []string
	for _, s := range a {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}
