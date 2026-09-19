package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// Checking a model against the warehouse it claims to describe.
//
// Everything else in this engine reasons about the model as written. Nothing
// asks whether the tables exist, whether the columns are still there, or
// whether a column the model calls a date is a string in the warehouse. A
// dropped column is currently found by a user getting an error, which is the
// most expensive place to find it and the last place anyone wants to.
//
// The check is deliberately read-only and metadata-only. It reads no rows, so
// it needs no more access than listing a schema, and it costs nothing to run
// on a schedule.

// Describer is an executor that can report what the warehouse actually holds.
//
// Optional: an executor that cannot introspect simply does not implement it,
// and doctor says so rather than pretending everything is fine. Reporting a
// model as healthy because nothing could check it is the failure this whole
// file exists to avoid.
type Describer interface {
	// Describe returns the columns of one table, keyed by lowercased name,
	// with the warehouse's own type as the value. The second return is false
	// when the table does not exist, which is not an error: a missing table is
	// a finding rather than a failure to look.
	Describe(ctx context.Context, source string) (map[string]string, bool, error)
}

// Finding is one thing wrong, or one thing worth knowing.
type Finding struct {
	// Severity is "error" for something that will break a query, and "warning"
	// for something that will not break today.
	Severity string `json:"severity"`
	// Dataset and Field name what the finding is about, in model terms.
	Dataset string `json:"dataset"`
	Field   string `json:"field,omitempty"`
	// Source is the physical table, for someone about to go and look.
	Source  string `json:"source,omitempty"`
	Message string `json:"message"`
	// Hint says what to do, when there is something to do.
	Hint string `json:"hint,omitempty"`
}

// Diagnosis is the whole report.
type Diagnosis struct {
	// Checked counts the tables actually inspected.
	Checked  int       `json:"tables_checked"`
	Findings []Finding `json:"findings"`
	// Skipped explains why nothing was checked, when nothing was.
	Skipped string `json:"skipped,omitempty"`
}

// OK reports whether anything will break.
func (d Diagnosis) OK() bool {
	for _, f := range d.Findings {
		if f.Severity == "error" {
			return false
		}
	}
	return true
}

// Doctor checks every table and column the model names against the warehouse.
//
// It reports rather than refuses: a model can be wrong in ways that do not
// matter yet, and deciding which of those to act on is a person's job. The
// severities are the opinion this offers.
func (e *Engine) Doctor(ctx context.Context) (Diagnosis, error) {
	out := Diagnosis{Findings: []Finding{}}

	if e.exec == nil {
		out.Skipped = "this engine has no executor, so there is no warehouse to check against"
		return out, nil
	}
	describer, ok := e.exec.(Describer)
	if !ok {
		out.Skipped = fmt.Sprintf(
			"%s cannot report what the warehouse holds, so the model was not checked against it",
			e.exec.Name())
		return out, nil
	}

	// One read per distinct table, not per field. A wide model would otherwise
	// make one metadata call per column.
	seen := map[string]bool{}
	for _, ns := range e.ws.Available() {
		for _, ds := range ns.Model.Datasets {
			source := strings.TrimSpace(ds.Source)
			if source == "" || seen[source] {
				continue
			}
			seen[source] = true
			out.Checked++
			out.Findings = append(out.Findings, checkDataset(ctx, describer, ns.Name, ds)...)
		}
	}

	sortFindings(out.Findings)
	return out, nil
}

func checkDataset(ctx context.Context, d Describer, namespace string, ds *osi.Dataset) []Finding {
	name := namespace + "." + ds.Name

	columns, exists, err := d.Describe(ctx, ds.Source)
	if err != nil {
		return []Finding{{
			Severity: "error", Dataset: name, Source: ds.Source,
			Message: "could not read this table: " + err.Error(),
			Hint:    "check the engine's credentials can read table metadata in this project",
		}}
	}
	if !exists {
		return []Finding{{
			Severity: "error", Dataset: name, Source: ds.Source,
			Message: "this table does not exist",
			Hint:    "every query touching this dataset fails; fix the source or drop the dataset",
		}}
	}

	var out []Finding

	// The primary key is what grain is derived from, so a key naming a column
	// that is not there means fan-out detection is running on a fiction.
	for _, key := range ds.PrimaryKey {
		if _, ok := columns[strings.ToLower(key)]; !ok {
			out = append(out, Finding{
				Severity: "error", Dataset: name, Field: key, Source: ds.Source,
				Message: "the primary key names a column this table does not have",
				Hint: "grain is derived from the primary key, so fan-out detection is" +
					" comparing against a column that does not exist",
			})
		}
	}

	for _, f := range ds.Fields {
		// Only a field that reads one column can be checked. An expression
		// over several is the emitter's business and the warehouse will judge
		// it when the query runs.
		column := soleColumn(f)
		if column == "" {
			continue
		}
		actual, ok := columns[strings.ToLower(column)]
		if !ok {
			out = append(out, Finding{
				Severity: "error", Dataset: name, Field: f.Name, Source: ds.Source,
				Message: fmt.Sprintf("reads column %q, which this table does not have", column),
				Hint:    "every query using this field fails",
			})
			continue
		}
		if msg := typeMismatch(f.Datatype, actual); msg != "" {
			out = append(out, Finding{
				Severity: "warning", Dataset: name, Field: f.Name, Source: ds.Source,
				Message: msg,
				Hint: "the warehouse decides, so filters and grains on this field may" +
					" behave differently than the model implies",
			})
		}
	}
	return out
}

// soleColumn returns the column a field reads, when it reads exactly one.
//
// The heuristic is deliberately strict: a bare identifier and nothing else.
// Anything with a function call, an operator or a dot is left alone rather
// than guessed at, because a wrong guess here produces a finding that sends
// somebody looking for a column that was never missing.
func soleColumn(f *osi.Field) string {
	expr := strings.TrimSpace(f.Expression.ANSI())
	if expr == "" {
		return f.Name
	}
	if strings.ContainsAny(expr, "(). +-*/,'\"") || strings.Contains(expr, " ") {
		return ""
	}
	return expr
}

// typeMismatch reports a declared type the warehouse contradicts.
//
// Only the differences that change behaviour are reported. A model calling a
// column a String when the warehouse says STRING is not news, and a report
// full of those is a report nobody reads.
func typeMismatch(declared, actual string) string {
	if declared == "" {
		return ""
	}
	want := family(osi.Datatype(declared))
	got := warehouseFamily(actual)
	if want == "" || got == "" || want == got {
		return ""
	}
	return fmt.Sprintf("declared %s, but the warehouse says %s", declared, actual)
}

// family reduces a declared type to what matters for planning: is it a
// number, a date, a timestamp, a boolean or text.
func family(d osi.Datatype) string {
	switch d {
	case osi.TypeInteger, osi.TypeDecimal, osi.TypeFloat:
		return "number"
	case osi.TypeDate:
		return "date"
	case osi.TypeTime, osi.TypeDateTime, osi.TypeDateTimeTz:
		return "time"
	case osi.TypeBoolean:
		return "boolean"
	case osi.TypeString:
		return "text"
	}
	return ""
}

func warehouseFamily(t string) string {
	upper := strings.ToUpper(strings.TrimSpace(t))
	switch {
	case upper == "DATE":
		return "date"
	case strings.Contains(upper, "TIMESTAMP"), strings.Contains(upper, "DATETIME"), upper == "TIME":
		return "time"
	case upper == "BOOL", upper == "BOOLEAN":
		return "boolean"
	case strings.Contains(upper, "INT"), strings.Contains(upper, "NUMERIC"),
		strings.Contains(upper, "DECIMAL"), strings.Contains(upper, "FLOAT"),
		strings.Contains(upper, "DOUBLE"), strings.Contains(upper, "REAL"):
		return "number"
	case strings.Contains(upper, "CHAR"), strings.Contains(upper, "STRING"), upper == "TEXT":
		return "text"
	}
	return ""
}

// sortFindings puts errors first, then groups by dataset, so the top of the
// report is the part that breaks something.
func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return f[i].Severity == "error"
		}
		if f[i].Dataset != f[j].Dataset {
			return f[i].Dataset < f[j].Dataset
		}
		return f[i].Field < f[j].Field
	})
}
