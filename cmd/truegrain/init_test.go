package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/introspect"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// What init writes has to load.
//
// The renderer builds YAML with fmt.Fprintf, so the failure mode is a file that
// looks right and will not parse, or parses into a model missing the keys that
// make fan-out detection possible. A command whose entire job is producing a
// working model repository is worthless if its output does not survive
// `truegrain validate`, so that is what this asserts: generate, load, check the
// grain and the join survived the round trip.

func sampleSchema() introspect.Schema {
	return introspect.Schema{
		Name: "retail",
		Tables: []introspect.Table{
			{
				Name:       "orders",
				PrimaryKey: []string{"order_id"},
				// A description with a colon, a quote and a newline, because
				// warehouse descriptions are written by whoever made the table.
				Description: "Orders: one row per order, \"confirmed\" only.\nSee the wiki.",
				Columns: []introspect.Column{
					{Name: "order_id", Type: "STRING"},
					{Name: "customer_id", Type: "STRING"},
					{Name: "total", Type: "NUMERIC", Description: "Gross: before tax"},
					{Name: "ordered_at", Type: "TIMESTAMP"},
				},
			},
			{
				Name:       "order_lines",
				PrimaryKey: []string{"order_id", "line_number"},
				Columns: []introspect.Column{
					{Name: "order_id", Type: "STRING"},
					{Name: "line_number", Type: "INT64"},
					{Name: "quantity", Type: "INT64"},
				},
			},
		},
		Relationships: []introspect.Relationship{{
			Name:        "lines_order",
			From:        "order_lines",
			FromColumns: []string{"order_id"},
			To:          "orders",
			ToColumns:   []string{"order_id"},
		}},
	}
}

func TestTheGeneratedRepositoryLoads(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeRepository(dir, "retail", "bigquery", "some-project", "US", "", sampleSchema(), false); err != nil {
		t.Fatalf("writing the repository: %v", err)
	}

	ws, err := workspace.Load(dir, workspace.Options{Strict: true})
	if err != nil {
		t.Fatalf("the generated model does not load: %v", err)
	}
	if ws.Legacy {
		t.Fatal("the namespace manifest was not found, so the layout is wrong")
	}

	ns, ok := ws.Namespace("retail")
	if !ok {
		t.Fatalf("no retail namespace; got %d namespace(s)", len(ws.Namespaces()))
	}

	// The keys are the whole point. Columns alone produce a model that compiles
	// and cannot tell that a join through order_lines multiplies rows.
	var lines bool
	for _, d := range ns.Model.Datasets {
		if d.Name != "order_lines" {
			continue
		}
		lines = true
		if got := strings.Join(d.PrimaryKey, ","); got != "order_id,line_number" {
			t.Errorf("compound key lost its order or its columns: %q", got)
		}
	}
	if !lines {
		t.Error("order_lines did not survive the round trip")
	}

	if len(ns.Model.Relationships) != 1 {
		t.Fatalf("want the declared foreign key, got %d relationships", len(ns.Model.Relationships))
	}
	if r := ns.Model.Relationships[0]; r.From != "order_lines" || r.To != "orders" {
		t.Errorf("the relationship lost its direction: %s -> %s", r.From, r.To)
	}
}

// TestMetricsAreNeverOverwritten.
//
// -force exists to pick up a schema change, and somebody will reach for it with
// a week of metric definitions in the same directory. Losing those would be a
// worse failure than anything init prevents.
func TestMetricsAreNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeRepository(dir, "retail", "bigquery", "some-project", "", "", sampleSchema(), false); err != nil {
		t.Fatal(err)
	}

	metrics := filepath.Join(dir, "models", "retail", "metrics.yaml")
	mine := "version: 0.2.0.dev0\nsemantic_model:\n  - name: retail\n    metrics: []\n# mine\n"
	if err := os.WriteFile(metrics, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeRepository(dir, "retail", "bigquery", "some-project", "", "", sampleSchema(), true); err != nil {
		t.Fatalf("regenerating: %v", err)
	}

	after, err := os.ReadFile(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != mine {
		t.Errorf("-force overwrote hand-written metrics:\n%s", after)
	}
}

// TestRegeneratingNeedsForce, so a re-run cannot quietly discard an edit
// somebody made to the generated file before reading the header.
func TestRegeneratingNeedsForce(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeRepository(dir, "retail", "bigquery", "some-project", "", "", sampleSchema(), false); err != nil {
		t.Fatal(err)
	}
	_, err := writeRepository(dir, "retail", "bigquery", "some-project", "", "", sampleSchema(), false)
	if err == nil {
		t.Fatal("a second run without -force must refuse rather than overwrite")
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Errorf("the refusal must say how to proceed: %v", err)
	}
}

// TestADatasetNameIsNotInterpolatedBlindly.
//
// The dataset reaches a query as text between backticks, so a backtick in the
// value would close the one being built. BigQuery's own quoting is not a
// defence here, which is why the rule is refusal rather than escaping.
func TestADatasetNameIsNotInterpolatedBlindly(t *testing.T) {
	for _, bad := range []string{
		"",
		"retail`.`other",
		"retail; DROP",
		"retail-prod",
		"retail.public",
		"retail ",
	} {
		if err := introspect.ValidateDatasetID(bad); err == nil {
			t.Errorf("%q was accepted as a dataset name", bad)
		}
	}
	if err := introspect.ValidateDatasetID("retail_prod_2"); err != nil {
		t.Errorf("a plain identifier was rejected: %v", err)
	}
}

// The warehouse menu.
//
// `init` used to ask "Warehouse [bigquery]:" as free text, which never said
// what was on offer and turned a typo into a refusal rather than a second
// chance. These cover the gcloud-shaped replacement.

func chose(t *testing.T, typed string) (string, error) {
	t.Helper()
	return chooseWarehouse(bufio.NewReader(strings.NewReader(typed)))
}

func TestANumberPicksAWarehouse(t *testing.T) {
	for typed, want := range map[string]string{
		"1\n": "bigquery",
		"2\n": "postgres",
	} {
		got, err := chose(t, typed)
		if err != nil {
			t.Errorf("%q: %v", typed, err)
			continue
		}
		if got != want {
			t.Errorf("%q selected %q, want %q", typed, got, want)
		}
	}
}

// TestANameWorksToo, because somebody who has done this before types the name,
// and somebody following a runbook pastes it.
func TestANameWorksToo(t *testing.T) {
	for _, typed := range []string{"postgres\n", "PostgreSQL\n", "POSTGRES\n"} {
		got, err := chose(t, typed)
		if err != nil {
			t.Errorf("%q: %v", typed, err)
			continue
		}
		if got != "postgres" {
			t.Errorf("%q selected %q, want postgres", typed, got)
		}
	}
}

func TestEnterTakesTheDefault(t *testing.T) {
	got, err := chose(t, "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "bigquery" {
		t.Errorf("want the first readable warehouse, got %q", got)
	}
}

// TestABadAnswerAsksAgain rather than exiting. Making somebody re-run the
// command because they typed 9 is the thing that makes a CLI feel hostile.
func TestABadAnswerAsksAgain(t *testing.T) {
	for _, typed := range []string{
		"9\n2\n",        // a number off the end of the list
		"0\n2\n",        // and one before the start
		"mysql\n2\n",    // a warehouse that is not here at all
		"\x00junk\n2\n", // something that is not a word
	} {
		got, err := chose(t, typed)
		if err != nil {
			t.Errorf("%q: %v", typed, err)
			continue
		}
		if got != "postgres" {
			t.Errorf("%q ended at %q, want postgres after the retry", typed, got)
		}
	}
}

// TestAWarehouseThatCannotBeReadSaysWhy.
//
// Snowflake runs queries but has no schema reader. Meeting that halfway
// through a setup, after entering an account and a key, is the wrong moment to
// find out.
func TestAWarehouseThatCannotBeReadSaysWhy(t *testing.T) {
	_, err := chose(t, "snowflake\n")
	if err == nil {
		t.Fatal("choosing a warehouse with no reader must not succeed")
	}
	if !strings.Contains(err.Error(), "Snowflake") {
		t.Errorf("the refusal should name the warehouse: %v", err)
	}
	if !strings.Contains(err.Error(), "reader") {
		t.Errorf("the refusal should say what is missing: %v", err)
	}
}

// TestAnUnreadableWarehouseHasNoNumber, so it cannot be chosen by accident by
// somebody counting down the list.
func TestAnUnreadableWarehouseHasNoNumber(t *testing.T) {
	readable := 0
	for _, w := range warehouses {
		if w.why == "" {
			readable++
		}
	}
	got, err := chose(t, strconv.Itoa(readable+1)+"\n2\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "postgres" {
		t.Errorf("a number past the readable options selected %q", got)
	}
}

// TestNoAnswerAndNothingToReadRefusesRatherThanHanging.
//
// The bug this guards is the one that takes a pipeline down. Closed stdin
// means ReadString returns EOF forever, so a prompt loop spins until somebody
// kills it, and the job looks hung rather than broken.
func TestNoAnswerAndNothingToReadRefusesRatherThanHanging(t *testing.T) {
	done := make(chan struct{})
	var err error
	go func() {
		_, err = chose(t, "")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("chooseWarehouse spun on closed stdin instead of refusing")
	}

	if err == nil {
		t.Fatal("closed stdin with no answer must refuse")
	}
	// The message has to name the flag, or somebody reads "no warehouse given"
	// and re-runs the same command in the same pipeline.
	if !strings.Contains(err.Error(), "-dialect") {
		t.Errorf("the refusal should name the flag that fixes it: %v", err)
	}
}

// TestBadInputThenNothingStillRefuses. The retry loop must reach the same
// terminating branch when the pipe had content but no valid answer.
func TestBadInputThenNothingStillRefuses(t *testing.T) {
	done := make(chan struct{})
	var err error
	go func() {
		_, err = chose(t, "nonsense\nmore nonsense\n")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("chooseWarehouse spun after exhausting a pipe")
	}
	if err == nil {
		t.Fatal("a pipe with no valid answer must refuse")
	}
}
