package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
)

// Checking a model against the warehouse it claims to describe.
//
// Everything else validates the model against itself. None of it notices that
// somebody dropped a column, and the first sign of that today is a caller
// getting an error.

// fakeWarehouse answers with whatever the test says the warehouse holds.
type fakeWarehouse struct {
	tables map[string]map[string]string
	err    error
}

func (f *fakeWarehouse) Name() string { return "fake" }
func (f *fakeWarehouse) Close() error { return nil }
func (f *fakeWarehouse) Execute(context.Context, govern.Identity, string, []any) (*engine.Rows, error) {
	return &engine.Rows{}, nil
}

func (f *fakeWarehouse) Describe(_ context.Context, source string) (map[string]string, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	cols, ok := f.tables[source]
	return cols, ok, nil
}

// blindWarehouse can run queries and cannot say what it holds.
//
// Declared standalone rather than embedding fakeWarehouse: embedding promotes
// Describe, which would make the blind one satisfy Describer and quietly test
// nothing.
type blindWarehouse struct{}

func (b *blindWarehouse) Name() string { return "blind warehouse" }
func (b *blindWarehouse) Close() error { return nil }
func (b *blindWarehouse) Execute(context.Context, govern.Identity, string, []any) (*engine.Rows, error) {
	return &engine.Rows{}, nil
}

func doctorWith(t *testing.T, ex engine.Executor) engine.Diagnosis {
	t.Helper()
	eng, err := engine.New(engine.Config{
		ModelPath: "../../testdata/workspace",
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
		Executor:  ex,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := eng.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// healthy is the fixture workspace as the warehouse would really have it.
func healthy() map[string]map[string]string {
	return map[string]map[string]string{
		"main.orders": {
			"order_id": "BIGINT", "customer_id": "BIGINT", "order_date": "DATE",
			"status": "VARCHAR", "order_total": "DECIMAL(12,2)",
		},
		"main.order_lines": {
			"order_id": "BIGINT", "line_number": "BIGINT", "item_id": "VARCHAR",
			"quantity": "BIGINT", "line_amount": "DECIMAL(12,2)",
		},
		"main.customers": {
			"customer_id": "BIGINT", "region": "VARCHAR",
			"signup_date": "DATE", "email": "VARCHAR",
		},
		"main.campaigns": {
			"campaign_id": "BIGINT", "order_id": "BIGINT",
			"channel": "VARCHAR", "spend": "DECIMAL(12,2)",
		},
	}
}

func TestAModelTheWarehouseAgreesWith(t *testing.T) {
	report := doctorWith(t, &fakeWarehouse{tables: healthy()})

	if !report.OK() {
		t.Errorf("a matching model should be healthy: %+v", report.Findings)
	}
	if report.Checked == 0 {
		t.Error("nothing was checked")
	}
}

func TestAMissingTableIsAnError(t *testing.T) {
	tables := healthy()
	delete(tables, "main.customers")

	report := doctorWith(t, &fakeWarehouse{tables: tables})
	if report.OK() {
		t.Fatal("a missing table must not be reported as healthy")
	}

	var found bool
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "does not exist") && f.Source == "main.customers" {
			found = true
			if f.Severity != "error" {
				t.Errorf("a missing table breaks every query touching it, got %q", f.Severity)
			}
		}
	}
	if !found {
		t.Errorf("the missing table was not reported: %+v", report.Findings)
	}
}

func TestAMissingColumnIsAnError(t *testing.T) {
	tables := healthy()
	delete(tables["main.orders"], "order_total")

	report := doctorWith(t, &fakeWarehouse{tables: tables})
	if report.OK() {
		t.Fatal("a missing column must not be reported as healthy")
	}
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "order_total") && f.Severity != "error" {
			t.Errorf("want error, got %q", f.Severity)
		}
	}
}

// TestAMissingPrimaryKeyColumnSaysWhyItMatters.
//
// Grain is derived from the primary key, so a key naming a column that is not
// there means fan-out detection is comparing against a fiction. That is worse
// than an ordinary missing column and the message has to say so.
func TestAMissingPrimaryKeyColumnSaysWhyItMatters(t *testing.T) {
	tables := healthy()
	delete(tables["main.order_lines"], "line_number")

	report := doctorWith(t, &fakeWarehouse{tables: tables})

	var explained bool
	for _, f := range report.Findings {
		if f.Field == "line_number" && strings.Contains(f.Hint, "grain") {
			explained = true
			if f.Severity != "error" {
				t.Errorf("want error, got %q", f.Severity)
			}
		}
	}
	if !explained {
		t.Errorf("a missing key column must explain the consequence: %+v", report.Findings)
	}
}

// TestATypeTheWarehouseContradictsIsAWarning, not an error: the query still
// runs, and what it means may have changed.
func TestATypeTheWarehouseContradictsIsAWarning(t *testing.T) {
	tables := healthy()
	tables["main.orders"]["order_date"] = "VARCHAR"

	report := doctorWith(t, &fakeWarehouse{tables: tables})

	var found bool
	for _, f := range report.Findings {
		if f.Field == "order_date" {
			found = true
			if f.Severity != "warning" {
				t.Errorf("a type difference does not break the query, got %q", f.Severity)
			}
		}
	}
	if !found {
		t.Errorf("a date column typed as text was not reported: %+v", report.Findings)
	}
	// Still healthy: nothing here stops a query running.
	if !report.OK() {
		t.Error("a warning alone must not fail the check")
	}
}

// TestAnEquivalentTypeIsNotReported. A report full of DECIMAL against Number
// is a report nobody reads, and then the real finding is in it somewhere.
func TestAnEquivalentTypeIsNotReported(t *testing.T) {
	tables := healthy()
	tables["main.orders"]["order_total"] = "NUMERIC"
	tables["main.customers"]["region"] = "STRING"

	report := doctorWith(t, &fakeWarehouse{tables: tables})
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "declared") {
			t.Errorf("an equivalent type was reported as a difference: %+v", f)
		}
	}
}

// TestAWarehouseThatCannotLookSaysSo.
//
// The failure this whole check exists to avoid is reporting a model as healthy
// because nothing could check it.
func TestAWarehouseThatCannotLookSaysSo(t *testing.T) {
	report := doctorWith(t, &blindWarehouse{})

	if report.Skipped == "" {
		t.Fatal("an executor that cannot introspect must say the model was not checked")
	}
	if !strings.Contains(report.Skipped, "blind warehouse") {
		t.Errorf("the reason should name the executor, got %q", report.Skipped)
	}
	if report.Checked != 0 {
		t.Errorf("nothing was checked, so the count must be zero, got %d", report.Checked)
	}
}

func TestACompileOnlyEngineIsNotReportedAsHealthy(t *testing.T) {
	eng, err := engine.New(engine.Config{
		ModelPath: "../../testdata/workspace",
		Dialect:   "duckdb",
		Resolver:  govern.AllowAll{},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := eng.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped == "" {
		t.Error("an engine with no executor has no warehouse to check against, and must say so")
	}
}

func TestErrorsSortAboveWarnings(t *testing.T) {
	tables := healthy()
	delete(tables["main.orders"], "order_total")
	tables["main.customers"]["signup_date"] = "VARCHAR"

	report := doctorWith(t, &fakeWarehouse{tables: tables})

	seenWarning := false
	for _, f := range report.Findings {
		if f.Severity == "warning" {
			seenWarning = true
		} else if seenWarning {
			t.Errorf("an error appeared below a warning:\n%+v", report.Findings)
			break
		}
	}
}
