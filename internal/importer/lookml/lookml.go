// Package lookml imports a Looker project into an Ossie model.
//
// # Why LookML is the best-case import
//
// A Looker join declares its own cardinality:
//
//	join: order_lines {
//	  relationship: one_to_many
//	  sql_on: ${orders.id} = ${order_lines.order_id} ;;
//	}
//
// That is the fan-out, stated by the person who wrote the model. dbt gives
// entities and the cardinality has to be derived from which side is
// primary; here it is written down. So an imported Looker project arrives
// knowing exactly which joins multiply rows, which is the single piece of
// information the refusal depends on.
//
// It also means the import can be pointed: a `one_to_many` join with a
// measure on the one side is a fan-out Looker itself handles with symmetric
// aggregates, a technique that works and is invisible, so the number was
// probably right and nobody knew why. This engine refuses instead and says
// so. That difference is worth understanding before comparing totals.
//
// # What is read
//
// `.view.lkml` files become datasets: `sql_table_name` is the source,
// `primary_key: yes` is the grain, dimensions become fields and measures
// become metrics. `.model.lkml` files contribute the join graph from their
// explores.
//
// # What is not
//
// Looker's templating is a language. `${TABLE}.col` and `${view.field}` are
// resolved because they are the common forms and mean something definite.
// Liquid, `{% if %}` and parameter interpolation are not: they produce
// different SQL per user or per dashboard, and a semantic layer whose
// definition depends on who is asking is the thing this project exists to
// replace. Anything containing them is reported and left out.
package lookml

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/introspect"
)

// Report says what happened, including what did not.
type Report struct {
	Project string
	Views   int
	Metrics int
	Joins   int
	// Skipped names what did not come across, with the reason.
	Skipped []string
	Notes   []string
}

// Import reads a directory of LookML files.
func Import(dir string) (introspect.Schema, Report, error) {
	files, err := collect(dir)
	if err != nil {
		return introspect.Schema{}, Report{}, err
	}
	if len(files) == 0 {
		return introspect.Schema{}, Report{}, fmt.Errorf(
			"no .lkml files under %s.\n"+
				"Point this at a Looker project directory: the views and models "+
				"are the .view.lkml and .model.lkml files in it", dir)
	}

	report := Report{Project: filepath.Base(dir)}
	schema := introspect.Schema{Name: "looker"}

	var views []*node
	var models []*node
	for _, f := range files {
		root, err := parse(f.body)
		if err != nil {
			return introspect.Schema{}, report, fmt.Errorf("%s: %w", f.path, err)
		}
		views = append(views, root.childrenOf("view")...)
		models = append(models, root.childrenOf("explore")...)
		// A .model.lkml holds explores at the top level; some projects nest
		// them under a model block.
		for _, m := range root.childrenOf("model") {
			models = append(models, m.childrenOf("explore")...)
		}
	}

	// Ordered, so importing twice produces the same file.
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })

	byView := map[string]*introspect.Table{}
	for _, v := range views {
		table, metrics, skipped := readView(v)
		if table == nil {
			report.Skipped = append(report.Skipped, skipped...)
			continue
		}
		schema.Tables = append(schema.Tables, *table)
		byView[strings.ToLower(table.Name)] = table
		schema.Metrics = append(schema.Metrics, metrics...)
		report.Skipped = append(report.Skipped, skipped...)
		report.Views++
	}
	report.Metrics = len(schema.Metrics)

	rels, joinSkipped, joinNotes := readJoins(models, byView)
	schema.Relationships = rels
	report.Joins = len(rels)
	report.Skipped = append(report.Skipped, joinSkipped...)
	report.Notes = append(report.Notes, joinNotes...)

	if prefix := commonSchema(views); prefix != "" {
		schema.Name = prefix
	}

	sort.Strings(report.Skipped)
	return introspect.Finish(schema), report, nil
}

type file struct {
	path string
	body string
}

func collect(dir string) ([]file, error) {
	var out []file
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".lkml") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, file{path: path, body: string(body)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading the Looker project at %s: %w", dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// readView turns one view into a dataset and its metrics.
func readView(v *node) (*introspect.Table, []introspect.Metric, []string) {
	var skipped []string

	if len(v.childrenOf("derived_table")) > 0 {
		// A derived table is SQL, often with Liquid in it, and pointing a
		// dataset at one would mean the model carried a query rather than a
		// table. Naming it is more useful than half-importing it.
		return nil, nil, []string{fmt.Sprintf(
			"view %s: it is a derived table, so it has no physical source to "+
				"point at. Materialise it in your warehouse and import that, or "+
				"write the dataset by hand", v.Name)}
	}

	source := v.get("sql_table_name")
	if source == "" {
		source = v.Name
	}
	if hasTemplating(source) {
		return nil, nil, []string{fmt.Sprintf(
			"view %s: sql_table_name contains templating (%s), so which table it "+
				"means depends on who is asking", v.Name, source)}
	}

	t := &introspect.Table{Name: v.Name, Description: v.get("description")}

	for _, d := range v.childrenOf("dimension") {
		col, ok := columnOf(d, v.Name)
		if !ok {
			skipped = append(skipped, fmt.Sprintf(
				"dimension %s.%s: its sql is an expression rather than a column "+
					"(%s), so it is left out. Add it by hand as a field with that "+
					"expression", v.Name, d.Name, d.get("sql")))
			continue
		}
		// The grain, stated by the person who wrote the model. Without it
		// every join into this view is assumed to fan out.
		if d.is("primary_key", "yes") {
			t.PrimaryKey = append(t.PrimaryKey, col)
		}
		t.Columns = append(t.Columns, introspect.Column{
			Name: col, Type: lookerType(d.get("type")), Nullable: true,
			Description: d.get("description"),
		})
	}

	// A dimension_group is one column presented at several granularities.
	// The column is what the model needs; the timeframes are a presentation
	// choice this engine makes with `grain` on the request instead.
	for _, g := range v.childrenOf("dimension_group") {
		col, ok := columnOf(g, v.Name)
		if !ok {
			skipped = append(skipped, fmt.Sprintf(
				"dimension_group %s.%s: its sql is an expression rather than a column",
				v.Name, g.Name))
			continue
		}
		t.Columns = append(t.Columns, introspect.Column{
			Name: col, Type: "timestamp", Nullable: true,
			Description: g.get("description"),
		})
	}

	metrics, measureSkipped := readMeasures(v)
	skipped = append(skipped, measureSkipped...)

	// Every column a metric or a join needs has to exist as a field, and a
	// measure's column is often not also a dimension.
	declared := map[string]bool{}
	for _, c := range t.Columns {
		declared[strings.ToLower(c.Name)] = true
	}
	for _, m := range v.childrenOf("measure") {
		if col, ok := columnOf(m, v.Name); ok && col != "" && !declared[strings.ToLower(col)] {
			declared[strings.ToLower(col)] = true
			t.Columns = append(t.Columns, introspect.Column{
				Name: col, Type: "number", Nullable: true,
			})
		}
	}

	t.Description = strings.TrimSpace(t.Description)
	return t, metrics, skipped
}

// readMeasures turns Looker measures into metrics.
func readMeasures(v *node) ([]introspect.Metric, []string) {
	var out []introspect.Metric
	var skipped []string

	for _, m := range v.childrenOf("measure") {
		kind := strings.ToLower(m.get("type"))

		// Three spellings, because LookML has three. `filters: [x: "y"]` is
		// a list, `filters: { field: x value: y }` is a block, and older
		// projects write a bare setting. Missing one imports the measure
		// without its filter, which gives it the same value as the
		// unfiltered one: a different number under the same name, silently.
		if len(m.childrenOf("filters")) > 0 || m.get("filters") != "" ||
			len(m.Lists["filters"]) > 0 {
			skipped = append(skipped, fmt.Sprintf(
				"measure %s.%s: it carries filters, which need a filtered "+
					"aggregate to reproduce exactly. Write it by hand as "+
					"SUM(CASE WHEN ... THEN ... END)", v.Name, m.Name))
			continue
		}

		// count with no sql is COUNT(*) over the view, which is well defined
		// only because the view has a grain.
		if kind == "count" && m.get("sql") == "" {
			out = append(out, introspect.Metric{
				Name: qualify(v.Name, m.Name), Description: m.get("description"),
				Expression: "COUNT(*)", Datatype: "Integer",
			})
			continue
		}

		col, ok := columnOf(m, v.Name)
		if !ok {
			skipped = append(skipped, fmt.Sprintf(
				"measure %s.%s: its sql is an expression (%s) rather than a "+
					"column, so the aggregation cannot be applied to a field. "+
					"Write it by hand", v.Name, m.Name, m.get("sql")))
			continue
		}

		expr, datatype, err := aggregate(kind, v.Name+"."+col)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("measure %s.%s: %v", v.Name, m.Name, err))
			continue
		}
		out = append(out, introspect.Metric{
			Name: qualify(v.Name, m.Name), Description: m.get("description"),
			Expression: expr, Datatype: datatype,
		})
	}
	return out, skipped
}

// qualify names a metric, prefixing the view when the measure's own name
// would be ambiguous across views.
//
// Looker addresses a measure as view_name.measure_name, and two views
// routinely both have `count`. Flattening them would make one silently win.
func qualify(view, measure string) string {
	if strings.EqualFold(measure, "count") || strings.EqualFold(measure, "total") {
		return view + "_" + measure
	}
	return measure
}

// aggregate renders a Looker measure type as ANSI SQL.
func aggregate(kind, col string) (string, string, error) {
	switch kind {
	case "sum", "sum_distinct":
		return "SUM(" + col + ")", "Number", nil
	case "count":
		return "COUNT(" + col + ")", "Integer", nil
	case "count_distinct":
		return "COUNT(DISTINCT " + col + ")", "Integer", nil
	case "average", "avg":
		return "AVG(" + col + ")", "Decimal", nil
	case "min":
		return "MIN(" + col + ")", "Number", nil
	case "max":
		return "MAX(" + col + ")", "Number", nil
	case "median", "percentile", "percentile_distinct":
		return "", "", fmt.Errorf(
			"%s has no portable ANSI form and differs by warehouse, so it is "+
				"not imported", kind)
	case "number", "string", "yesno", "date":
		// A type: number measure is an expression over other measures, which
		// is Looker's derived metric. It may compose measures at different
		// grains, which is what the fan-out check exists to catch.
		return "", "", fmt.Errorf(
			"type: %s is an expression over other measures, which may combine "+
				"grains. Write it by hand once you have checked they agree", kind)
	case "running_total", "period_over_period":
		return "", "", fmt.Errorf(
			"%s is a window function over an ordered frame, which this engine "+
				"refuses by name rather than approximating", kind)
	case "":
		return "", "", fmt.Errorf("it declares no type")
	}
	return "", "", fmt.Errorf("unrecognised measure type %q", kind)
}

// readJoins turns explores into relationships.
//
// This is the part worth getting right. Looker states cardinality on the
// join, so unlike every other importer nothing has to be inferred: a
// one_to_many is a fan-out and the model says so.
func readJoins(explores []*node, views map[string]*introspect.Table) ([]introspect.Relationship, []string, []string) {
	var out []introspect.Relationship
	var skipped, notes []string
	seen := map[string]bool{}

	for _, e := range explores {
		base := e.get("from")
		if base == "" {
			base = e.get("view_name")
		}
		if base == "" {
			base = e.Name
		}

		for _, j := range e.childrenOf("join") {
			to := j.get("from")
			if to == "" {
				to = j.Name
			}
			on := j.get("sql_on")
			if on == "" {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: it has no sql_on, so there is nothing to join on",
					base, to))
				continue
			}
			if hasTemplating(on) {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: its sql_on contains templating, so which rows "+
						"match depends on who is asking", base, to))
				continue
			}

			pairs, ok := joinColumns(on)
			if !ok {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: sql_on is not a simple equality between "+
						"columns (%s), so the join cannot be expressed as declared "+
						"key columns", base, to, collapse(on)))
				continue
			}

			rel, note, ok := orient(base, to, pairs, j.get("relationship"), views)
			if note != "" {
				notes = append(notes, note)
			}
			if !ok {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: relationship %q could not be oriented; the "+
						"engine derives fan-out from which side is unique, so a join "+
						"it cannot orient is left out rather than guessed",
					base, to, j.get("relationship")))
				continue
			}
			key := rel.From + "->" + rel.To
			if seen[key] {
				continue // the same join declared in two explores
			}
			seen[key] = true
			out = append(out, rel)
		}
	}
	return out, skipped, notes
}

// orient decides which side of the relationship is the one row.
//
// Ossie relationships point from the many side to the one side, which is
// what lets the planner see that joining the other way repeats rows. Looker
// says this directly, so this is a translation rather than an inference.
func orient(base, joined string, pairs [][2]string, relationship string,
	views map[string]*introspect.Table) (introspect.Relationship, string, bool) {

	rel := introspect.Relationship{Name: base + "_to_" + joined}

	from, to := base, joined
	switch strings.ToLower(relationship) {
	case "many_to_one", "":
		// The default in Looker, and the common case: many base rows point
		// at one joined row.
		from, to = base, joined
	case "one_to_many":
		// Each base row matches many joined rows, so the joined side is the
		// many side and points back.
		from, to = joined, base
	case "one_to_one":
		from, to = base, joined
	case "many_to_many":
		// Neither side is unique, so there is no key to declare and the
		// planner could not check anything. Left out rather than emitted as
		// a join that claims a grain it does not have.
		return rel, fmt.Sprintf(
			"join %s -> %s is many_to_many. Neither side is unique, so it is not "+
				"imported: a relationship the planner cannot check is worse than "+
				"none, because it would pass the fan-out check it should fail",
			base, joined), false
	default:
		return rel, "", false
	}

	rel.Name = from + "_to_" + to
	rel.From, rel.To = from, to
	for _, p := range pairs {
		a, b := p[0], p[1]
		// Each pair is view.column on both sides; assign each to whichever
		// side it names.
		fromCol, toCol := split(a, from), split(b, to)
		if fromCol == "" || toCol == "" {
			fromCol, toCol = split(b, from), split(a, to)
		}
		if fromCol == "" || toCol == "" {
			return rel, "", false
		}
		rel.FromColumns = append(rel.FromColumns, fromCol)
		rel.ToColumns = append(rel.ToColumns, toCol)
	}
	if len(rel.FromColumns) == 0 {
		return rel, "", false
	}
	return rel, "", true
}

// split returns the column when qualified reference names this view.
func split(reference, view string) string {
	v, col, ok := strings.Cut(reference, ".")
	if !ok {
		return ""
	}
	if !strings.EqualFold(v, view) {
		return ""
	}
	return col
}

// joinRef matches ${view.column} and ${TABLE}.column.
var joinRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)\}`)

// joinColumns reads the column pairs out of a sql_on.
//
// Only a conjunction of plain equalities. Anything else, a function call, an
// OR, a literal comparison, is a join condition that cannot be stated as
// declared key columns, and the caller reports it rather than approximating.
func joinColumns(on string) ([][2]string, bool) {
	clauses := splitOnAnd(on)
	if len(clauses) == 0 {
		return nil, false
	}

	var pairs [][2]string
	for _, clause := range clauses {
		left, right, ok := strings.Cut(clause, "=")
		if !ok {
			return nil, false
		}
		l := joinRef.FindStringSubmatch(strings.TrimSpace(left))
		r := joinRef.FindStringSubmatch(strings.TrimSpace(right))
		if l == nil || r == nil {
			return nil, false
		}
		// The whole side has to be the reference; `LOWER(${a.b}) = ${c.d}`
		// is a join on an expression and is not expressible here.
		if strings.TrimSpace(left) != l[0] || strings.TrimSpace(right) != r[0] {
			return nil, false
		}
		pairs = append(pairs, [2]string{l[1] + "." + l[2], r[1] + "." + r[2]})
	}
	return pairs, true
}

// splitOnAnd divides a condition on the word AND, case-insensitively.
func splitOnAnd(on string) []string {
	fields := regexp.MustCompile(`(?i)\sAND\s`).Split(on, -1)
	var out []string
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// columnOf reads the physical column a dimension or measure sits on.
//
// ${TABLE}.col is the ordinary form. A bare name is the column itself. An
// expression is not a column and the caller reports it.
func columnOf(n *node, view string) (string, bool) {
	sql := strings.TrimSpace(n.get("sql"))
	if sql == "" {
		// No sql means the field name is the column, which is Looker's own
		// default.
		return n.Name, true
	}
	if hasTemplating(sql) {
		return "", false
	}

	if rest, ok := strings.CutPrefix(sql, "${TABLE}."); ok {
		if isPlainColumn(rest) {
			return rest, true
		}
		return "", false
	}
	// ${view.field} pointing at this same view.
	if m := joinRef.FindStringSubmatch(sql); m != nil && m[0] == sql {
		if strings.EqualFold(m[1], view) {
			return m[2], true
		}
		return "", false
	}
	if isPlainColumn(sql) {
		return sql, true
	}
	return "", false
}

func isPlainColumn(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// hasTemplating reports Liquid or parameter interpolation.
//
// A definition that changes per user is the thing a governed semantic layer
// replaces, so one is reported rather than imported with whichever branch
// happened to be first.
func hasTemplating(s string) bool {
	return strings.Contains(s, "{%") || strings.Contains(s, "{{")
}

// lookerType maps a Looker dimension type onto something the writer can
// turn into an Ossie datatype.
func lookerType(t string) string {
	switch strings.ToLower(t) {
	case "number":
		return "number"
	case "yesno":
		return "boolean"
	case "date", "date_time", "time":
		return "timestamp"
	}
	return "string"
}

// commonSchema reads the schema every view shares, for the source prefix.
func commonSchema(views []*node) string {
	prefix := ""
	for _, v := range views {
		source := v.get("sql_table_name")
		if source == "" || hasTemplating(source) {
			continue
		}
		parts := strings.Split(source, ".")
		if len(parts) < 2 {
			continue
		}
		s := parts[len(parts)-2]
		if prefix == "" {
			prefix = s
		} else if !strings.EqualFold(prefix, s) {
			return "" // they disagree; the caller keeps the default
		}
	}
	return prefix
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
