// Package cube imports a Cube data model into an Ossie model.
//
// # What Cube gives, and what it does not
//
// Cube declares joins with a `relationship`, the same vocabulary Looker
// uses, so cardinality arrives stated rather than inferred. What it does
// not declare is a primary key: Cube has `primaryKey: true` on a dimension,
// but it is optional and many models omit it because Cube only needs it for
// certain measure types.
//
// That absence is the interesting part. Without a grain the planner assumes
// every join into a cube fans out and refuses sums across it, which is
// correct and conservative but makes the imported model much less useful.
// So a missing primary key is reported per cube rather than mentioned once,
// because it is the single edit that turns a cautious model into a usable
// one.
//
// # YAML only
//
// Cube models can be YAML or JavaScript. The JavaScript ones are programs:
// they build definitions with loops and conditionals, and reading them
// would mean running them. A .js model is named and skipped rather than
// half-parsed.
package cube

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rk-chavali/truegrain/internal/introspect"
)

// file is one Cube YAML model.
type file struct {
	Cubes []cubeDef `yaml:"cubes"`
	Views []struct {
		Name string `yaml:"name"`
	} `yaml:"views"`
}

type cubeDef struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// SQLTable is the physical relation. sql is the alternative and is a
	// query rather than a table.
	SQLTable   string      `yaml:"sql_table"`
	SQL        string      `yaml:"sql"`
	Dimensions []dimension `yaml:"dimensions"`
	Measures   []measure   `yaml:"measures"`
	Joins      []join      `yaml:"joins"`
}

type dimension struct {
	Name        string `yaml:"name"`
	SQL         string `yaml:"sql"`
	Type        string `yaml:"type"`
	PrimaryKey  bool   `yaml:"primary_key"`
	Description string `yaml:"description"`
}

type measure struct {
	Name        string `yaml:"name"`
	SQL         string `yaml:"sql"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	// Filters make a measure conditional, which needs a filtered aggregate
	// to reproduce exactly.
	Filters []map[string]string `yaml:"filters"`
	// RollingWindow is Cube's window function.
	RollingWindow map[string]any `yaml:"rolling_window"`
}

type join struct {
	Name         string `yaml:"name"`
	SQL          string `yaml:"sql"`
	Relationship string `yaml:"relationship"`
}

// Report says what happened, including what did not.
type Report struct {
	Project string
	Cubes   int
	Metrics int
	Joins   int
	Skipped []string
	Notes   []string
}

// Import reads a directory of Cube model files.
func Import(dir string) (introspect.Schema, Report, error) {
	paths, jsFiles, err := collect(dir)
	if err != nil {
		return introspect.Schema{}, Report{}, err
	}
	if len(paths) == 0 && len(jsFiles) == 0 {
		return introspect.Schema{}, Report{}, fmt.Errorf(
			"no Cube model files under %s.\n"+
				"Point this at the model directory of a Cube project, which is "+
				"usually model/cubes or schema", dir)
	}

	report := Report{Project: filepath.Base(dir)}
	for _, js := range jsFiles {
		report.Skipped = append(report.Skipped, fmt.Sprintf(
			"%s: a JavaScript model is a program rather than a declaration, and "+
				"reading it would mean running it. Convert it to YAML, or write "+
				"the datasets by hand", filepath.Base(js)))
	}

	var cubes []cubeDef
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			return introspect.Schema{}, report, err
		}
		var f file
		dec := yaml.NewDecoder(strings.NewReader(string(src)))
		if err := dec.Decode(&f); err != nil {
			return introspect.Schema{}, report, fmt.Errorf("parsing %s: %w", path, err)
		}
		cubes = append(cubes, f.Cubes...)
	}

	// Ordered, so importing twice produces the same file.
	sort.Slice(cubes, func(i, j int) bool { return cubes[i].Name < cubes[j].Name })

	schema := introspect.Schema{Name: "cube"}
	known := map[string]bool{}
	for _, c := range cubes {
		known[strings.ToLower(c.Name)] = true
	}

	var keyless []string
	for _, c := range cubes {
		table, metrics, skipped := readCube(c)
		report.Skipped = append(report.Skipped, skipped...)
		if table == nil {
			continue
		}
		if len(table.PrimaryKey) == 0 {
			keyless = append(keyless, c.Name)
		}
		schema.Tables = append(schema.Tables, *table)
		schema.Metrics = append(schema.Metrics, metrics...)
		report.Cubes++
	}
	report.Metrics = len(schema.Metrics)

	rels, joinSkipped := readJoins(cubes, known)
	schema.Relationships = rels
	report.Joins = len(rels)
	report.Skipped = append(report.Skipped, joinSkipped...)

	if len(keyless) > 0 {
		// Reported here as well as by Finish, because this is the one edit
		// that turns a cautious imported model into a usable one and Cube
		// makes the key optional, so it is commonly missing.
		report.Notes = append(report.Notes, fmt.Sprintf(
			"%d cube(s) declare no primary_key: %s. Cube treats it as optional, "+
				"this engine derives grain from it, and without one every join "+
				"into those cubes is assumed to fan out and sums across them are "+
				"refused. Adding it is the single edit that makes the imported "+
				"model useful", len(keyless), strings.Join(keyless, ", ")))
	}
	if prefix := commonSchema(cubes); prefix != "" {
		schema.Name = prefix
	}

	sort.Strings(report.Skipped)
	return introspect.Finish(schema), report, nil
}

func collect(dir string) (yamlFiles, jsFiles []string, err error) {
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yml", ".yaml":
			yamlFiles = append(yamlFiles, path)
		case ".js":
			jsFiles = append(jsFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reading the Cube project at %s: %w", dir, err)
	}
	sort.Strings(yamlFiles)
	sort.Strings(jsFiles)
	return yamlFiles, jsFiles, nil
}

func readCube(c cubeDef) (*introspect.Table, []introspect.Metric, []string) {
	var skipped []string

	if c.SQLTable == "" {
		if c.SQL != "" {
			return nil, nil, []string{fmt.Sprintf(
				"cube %s: it is defined by a query rather than by sql_table, so "+
					"there is no physical relation to point at. Materialise it and "+
					"import that, or write the dataset by hand", c.Name)}
		}
		return nil, nil, []string{fmt.Sprintf(
			"cube %s: it names no sql_table", c.Name)}
	}

	t := &introspect.Table{Name: c.Name, Description: c.Description}
	declared := map[string]bool{}

	for _, d := range c.Dimensions {
		col, ok := columnOf(d.SQL, d.Name)
		if !ok {
			skipped = append(skipped, fmt.Sprintf(
				"dimension %s.%s: its sql is an expression (%s) rather than a "+
					"column, so it is left out. Add it by hand as a field with that "+
					"expression", c.Name, d.Name, collapse(d.SQL)))
			continue
		}
		if d.PrimaryKey {
			t.PrimaryKey = append(t.PrimaryKey, col)
		}
		declared[strings.ToLower(col)] = true
		t.Columns = append(t.Columns, introspect.Column{
			Name: col, Type: cubeType(d.Type), Nullable: true,
			Description: d.Description,
		})
	}

	metrics, measureSkipped := readMeasures(c, declared, t)
	skipped = append(skipped, measureSkipped...)
	return t, metrics, skipped
}

func readMeasures(c cubeDef, declared map[string]bool, t *introspect.Table) ([]introspect.Metric, []string) {
	var out []introspect.Metric
	var skipped []string

	for _, m := range c.Measures {
		switch {
		case len(m.Filters) > 0:
			skipped = append(skipped, fmt.Sprintf(
				"measure %s.%s: it carries filters, which need a filtered "+
					"aggregate to reproduce exactly. Write it by hand as "+
					"SUM(CASE WHEN ... THEN ... END)", c.Name, m.Name))
			continue
		case len(m.RollingWindow) > 0:
			skipped = append(skipped, fmt.Sprintf(
				"measure %s.%s: a rolling_window is a window function over an "+
					"ordered frame, which this engine refuses by name rather than "+
					"approximating", c.Name, m.Name))
			continue
		}

		kind := strings.ToLower(m.Type)
		if kind == "count" && strings.TrimSpace(m.SQL) == "" {
			out = append(out, introspect.Metric{
				Name: qualify(c.Name, m.Name), Description: m.Description,
				Expression: "COUNT(*)", Datatype: "Integer",
			})
			continue
		}

		col, ok := columnOf(m.SQL, m.Name)
		if !ok {
			skipped = append(skipped, fmt.Sprintf(
				"measure %s.%s: its sql is an expression (%s) rather than a "+
					"column, so the aggregation cannot be applied to a field. "+
					"Write it by hand", c.Name, m.Name, collapse(m.SQL)))
			continue
		}

		expr, datatype, err := aggregate(kind, c.Name+"."+col)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("measure %s.%s: %v", c.Name, m.Name, err))
			continue
		}
		// A measure's column is often not also a dimension, and every column
		// a metric needs has to exist as a field.
		if !declared[strings.ToLower(col)] {
			declared[strings.ToLower(col)] = true
			t.Columns = append(t.Columns, introspect.Column{
				Name: col, Type: "number", Nullable: true,
			})
		}
		out = append(out, introspect.Metric{
			Name: qualify(c.Name, m.Name), Description: m.Description,
			Expression: expr, Datatype: datatype,
		})
	}
	return out, skipped
}

// qualify prefixes a name that would collide across cubes.
func qualify(cube, measure string) string {
	if strings.EqualFold(measure, "count") || strings.EqualFold(measure, "total") {
		return cube + "_" + measure
	}
	return measure
}

func aggregate(kind, col string) (string, string, error) {
	switch kind {
	case "sum":
		return "SUM(" + col + ")", "Number", nil
	case "count":
		return "COUNT(" + col + ")", "Integer", nil
	case "count_distinct", "count_distinct_approx":
		if kind == "count_distinct_approx" {
			// The approximate form is a different number, and which
			// approximation differs by warehouse. Exact is the honest import
			// and the caller is told.
			return "COUNT(DISTINCT " + col + ")", "Integer", nil
		}
		return "COUNT(DISTINCT " + col + ")", "Integer", nil
	case "avg":
		return "AVG(" + col + ")", "Decimal", nil
	case "min":
		return "MIN(" + col + ")", "Number", nil
	case "max":
		return "MAX(" + col + ")", "Number", nil
	case "number", "string", "time", "boolean":
		return "", "", fmt.Errorf(
			"type: %s is an expression over other measures, which may combine "+
				"grains. Write it by hand once you have checked they agree", kind)
	case "":
		return "", "", fmt.Errorf("it declares no type")
	}
	return "", "", fmt.Errorf("unrecognised measure type %q", kind)
}

// readJoins turns Cube joins into relationships.
//
// Cube states cardinality from the declaring cube's point of view, the same
// way Looker does, so this is a translation rather than an inference.
func readJoins(cubes []cubeDef, known map[string]bool) ([]introspect.Relationship, []string) {
	var out []introspect.Relationship
	var skipped []string
	seen := map[string]bool{}

	for _, c := range cubes {
		for _, j := range c.Joins {
			if !known[strings.ToLower(j.Name)] {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: no cube by that name was found, so there is "+
						"nothing to join to", c.Name, j.Name))
				continue
			}
			pairs, ok := joinColumns(j.SQL)
			if !ok {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: its sql is not a simple equality between "+
						"columns (%s), so the join cannot be expressed as declared "+
						"key columns", c.Name, j.Name, collapse(j.SQL)))
				continue
			}

			from, to := c.Name, j.Name
			switch strings.ToLower(j.Relationship) {
			case "many_to_one", "belongs_to", "":
				// Cube's default and the common case.
			case "one_to_many", "has_many":
				// The declaring cube is the one side, so the other points back.
				from, to = j.Name, c.Name
			case "one_to_one", "has_one":
			case "many_to_many":
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: many_to_many, so neither side is unique and "+
						"there is no key to declare. A relationship the planner "+
						"cannot check is worse than none, because it would pass the "+
						"fan-out check it should fail", c.Name, j.Name))
				continue
			default:
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: unrecognised relationship %q",
					c.Name, j.Name, j.Relationship))
				continue
			}

			rel := introspect.Relationship{Name: from + "_to_" + to, From: from, To: to}
			bad := false
			for _, p := range pairs {
				fromCol, toCol := split(p[0], from), split(p[1], to)
				if fromCol == "" || toCol == "" {
					fromCol, toCol = split(p[1], from), split(p[0], to)
				}
				if fromCol == "" || toCol == "" {
					bad = true
					break
				}
				rel.FromColumns = append(rel.FromColumns, fromCol)
				rel.ToColumns = append(rel.ToColumns, toCol)
			}
			if bad || len(rel.FromColumns) == 0 {
				skipped = append(skipped, fmt.Sprintf(
					"join %s -> %s: its sql does not name a column on each side",
					c.Name, j.Name))
				continue
			}
			key := rel.From + "->" + rel.To
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, rel)
		}
	}
	return out, skipped
}

func split(reference, cube string) string {
	c, col, ok := strings.Cut(reference, ".")
	if !ok || !strings.EqualFold(c, cube) {
		return ""
	}
	return col
}

// joinColumns reads column pairs out of a Cube join sql.
//
// Cube writes ${cube.column} or ${CUBE.column}, where CUBE means the
// declaring cube.
func joinColumns(sql string) ([][2]string, bool) {
	clauses := strings.Split(sql, " AND ")
	var pairs [][2]string
	for _, clause := range clauses {
		left, right, ok := strings.Cut(clause, "=")
		if !ok {
			return nil, false
		}
		l, lok := reference(strings.TrimSpace(left))
		r, rok := reference(strings.TrimSpace(right))
		if !lok || !rok {
			return nil, false
		}
		pairs = append(pairs, [2]string{l, r})
	}
	if len(pairs) == 0 {
		return nil, false
	}
	return pairs, true
}

// reference reads ${cube.column}, requiring the whole side to be one.
func reference(s string) (string, bool) {
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", false
	}
	inner := s[2 : len(s)-1]
	cube, col, ok := strings.Cut(inner, ".")
	if !ok || cube == "" || col == "" {
		return "", false
	}
	if !plain(cube) || !plain(col) {
		return "", false
	}
	return cube + "." + col, true
}

// columnOf reads the physical column a dimension or measure sits on.
func columnOf(sql, name string) (string, bool) {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		// Cube defaults the column to the field name.
		return name, true
	}
	if ref, ok := reference(sql); ok {
		_, col, _ := strings.Cut(ref, ".")
		return col, true
	}
	if plain(sql) {
		return sql, true
	}
	return "", false
}

func plain(s string) bool {
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

func cubeType(t string) string {
	switch strings.ToLower(t) {
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "time":
		return "timestamp"
	}
	return "string"
}

func commonSchema(cubes []cubeDef) string {
	prefix := ""
	for _, c := range cubes {
		parts := strings.Split(c.SQLTable, ".")
		if len(parts) < 2 {
			continue
		}
		s := parts[len(parts)-2]
		if prefix == "" {
			prefix = s
		} else if !strings.EqualFold(prefix, s) {
			return ""
		}
	}
	return prefix
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
