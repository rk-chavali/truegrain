package introspect

import (
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// Reading a warehouse goes wrong quietly, which is why these are here.
//
// The first version read constraints from INFORMATION_SCHEMA by joining
// KEY_COLUMN_USAGE to CONSTRAINT_COLUMN_USAGE on the constraint name, and the
// first live run reported a two-column key as
// "order_id, order_id, line_number, line_number": a fan-out inside the tool
// whose job is detecting fan-outs. It looked like a working command until
// somebody read the output.
//
// These cover the translation from BigQuery's own metadata, which is where that
// class of mistake now lives.

func TestACompoundPrimaryKeyArrivesOnceAndInOrder(t *testing.T) {
	md := &bigquery.TableMetadata{
		Schema: bigquery.Schema{
			{Name: "order_id", Type: bigquery.StringFieldType, Required: true},
			{Name: "line_number", Type: bigquery.IntegerFieldType, Required: true},
		},
		TableConstraints: &bigquery.TableConstraints{
			PrimaryKey: &bigquery.PrimaryKey{Columns: []string{"order_id", "line_number"}},
		},
	}

	got, skipped := describeTable("order_lines", md)

	if len(skipped) != 0 {
		t.Errorf("a scalar column was skipped: %v", skipped)
	}

	if key := strings.Join(got.PrimaryKey, ","); key != "order_id,line_number" {
		t.Errorf("want the key once and in declared order, got %q", key)
	}
	if len(got.Columns) != 2 {
		t.Errorf("want two columns, got %d", len(got.Columns))
	}
	if got.Columns[0].Nullable {
		t.Error("a REQUIRED column was read as nullable")
	}
}

// TestACompoundForeignKeyKeepsItsPairing.
//
// FromColumns and ToColumns are positional, so crossing them produces a join
// that runs and returns the wrong rows. The columns are deliberately given in
// an order that does not match the referenced table's own key, because the API
// pairs them explicitly and nothing here is entitled to reorder either side.
func TestACompoundForeignKeyKeepsItsPairing(t *testing.T) {
	md := &bigquery.TableMetadata{
		TableConstraints: &bigquery.TableConstraints{
			ForeignKeys: []*bigquery.ForeignKey{{
				Name:            "ships_line",
				ReferencedTable: &bigquery.Table{DatasetID: "main", TableID: "order_lines"},
				ColumnReferences: []*bigquery.ColumnReference{
					{ReferencingColumn: "line_no", ReferencedColumn: "line_number"},
					{ReferencingColumn: "ord_id", ReferencedColumn: "order_id"},
				},
			}},
		},
	}

	got := relationshipsOf("shipments", "main", md)

	if len(got) != 1 {
		t.Fatalf("want one relationship, got %d", len(got))
	}
	if from := strings.Join(got[0].FromColumns, ","); from != "line_no,ord_id" {
		t.Errorf("from columns were reordered: %q", from)
	}
	if to := strings.Join(got[0].ToColumns, ","); to != "line_number,order_id" {
		t.Errorf("the pairing was lost: %q", to)
	}
}

// TestAForeignKeyOutOfTheDatasetIsSkipped, because the generated model has no
// dataset to join to and would fail validation naming one that does not exist.
func TestAForeignKeyOutOfTheDatasetIsSkipped(t *testing.T) {
	md := &bigquery.TableMetadata{
		TableConstraints: &bigquery.TableConstraints{
			ForeignKeys: []*bigquery.ForeignKey{{
				Name:            "lines_order",
				ReferencedTable: &bigquery.Table{DatasetID: "elsewhere", TableID: "orders"},
				ColumnReferences: []*bigquery.ColumnReference{
					{ReferencingColumn: "order_id", ReferencedColumn: "order_id"},
				},
			}},
		},
	}

	if got := relationshipsOf("order_lines", "main", md); len(got) != 0 {
		t.Errorf("a join to another dataset was emitted: %+v", got)
	}
}

// TestAViewIsReadButHasNoGrain. A view carries no constraints, so it arrives
// keyless, and saying so is the point: a guessed key would pass the fan-out
// check and inflate a sum with complete confidence.
func TestAViewIsReadButHasNoGrain(t *testing.T) {
	md := &bigquery.TableMetadata{
		Type:   bigquery.ViewTable,
		Schema: bigquery.Schema{{Name: "order_id", Type: bigquery.StringFieldType}},
	}

	got, _ := describeTable("recent_orders", md)

	if len(got.Columns) != 1 {
		t.Fatalf("the view's columns were dropped: %+v", got)
	}
	if len(got.PrimaryKey) != 0 {
		t.Errorf("a view was given a key it does not have: %v", got.PrimaryKey)
	}
}

func TestAKeylessTableIsReportedRatherThanGuessed(t *testing.T) {
	s := Finish(Schema{Name: "main", Tables: []Table{
		{Name: "events"},
		{Name: "orders", PrimaryKey: []string{"order_id"}},
	}})

	if got := s.TablesWithoutKeys(); len(got) != 1 || got[0] != "events" {
		t.Errorf("want events named, got %v", got)
	}
	if !containing(s.Notes, "fan-out") {
		t.Errorf("the note must say what is lost, not just that a key is missing: %v", s.Notes)
	}
	if !containing(s.Notes, "no foreign keys") {
		t.Errorf("a model where nothing can be joined must say so: %v", s.Notes)
	}
}

// TestTheOrderIsStable, so regenerating after a schema change is a readable
// diff rather than a reshuffle. Table listing order is the warehouse's to
// choose and it does not have to be the same twice.
func TestTheOrderIsStable(t *testing.T) {
	s := Finish(Schema{
		Name:   "main",
		Tables: []Table{{Name: "orders"}, {Name: "campaigns"}, {Name: "customers"}},
		Relationships: []Relationship{
			{Name: "orders_customer", From: "orders", To: "customers"},
			{Name: "campaigns_order", From: "campaigns", To: "orders"},
		},
	})

	var names []string
	for _, table := range s.Tables {
		names = append(names, table.Name)
	}
	if got := strings.Join(names, ","); got != "campaigns,customers,orders" {
		t.Errorf("tables are not sorted: %q", got)
	}
	if s.Relationships[0].From != "campaigns" {
		t.Errorf("relationships are not sorted: %+v", s.Relationships)
	}
}

// TestTheExampleMetricIsNotAKey.
//
// An integer foreign key is numeric, and the first version suggested
// SUM(order_id) as somebody's opening metric. The example is the first thing
// read, so it teaches whatever it shows.
func TestTheExampleMetricIsNotAKey(t *testing.T) {
	s := Finish(Schema{
		Name: "main",
		Tables: []Table{{
			Name:       "campaigns",
			PrimaryKey: []string{"campaign_id"},
			Columns: []Column{
				{Name: "campaign_id", Type: "INTEGER"},
				{Name: "order_id", Type: "INTEGER"},
				{Name: "spend", Type: "NUMERIC"},
			},
		}, {
			Name:       "orders",
			PrimaryKey: []string{"order_id"},
			Columns:    []Column{{Name: "order_id", Type: "INTEGER"}},
		}},
		Relationships: []Relationship{{
			Name: "campaigns_order", From: "campaigns", FromColumns: []string{"order_id"},
			To: "orders", ToColumns: []string{"order_id"},
		}},
	})

	got := exampleMetric(s)

	if !strings.Contains(got, "spend") {
		t.Errorf("want the measure, got:\n%s", got)
	}
	if strings.Contains(got, "order_id") || strings.Contains(got, "campaign_id") {
		t.Errorf("a key column was suggested as a metric:\n%s", got)
	}
}

// TestADescriptionCannotBreakTheFile.
//
// Warehouse descriptions are written by whoever created the table and contain
// colons, quotes and newlines often enough that emitting them bare produces a
// file that will not parse, which is a bad first impression for a command whose
// entire job is producing a file that does.
func TestADescriptionCannotBreakTheFile(t *testing.T) {
	got := quote("Orders: one row per \"confirmed\" order.\nSee the wiki.\x00")

	for _, bad := range []string{"\n", "\x00"} {
		if strings.Contains(got, bad) {
			t.Errorf("a control character survived quoting: %q", got)
		}
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("the value was not quoted: %q", got)
	}
	if strings.Count(got, `"`) != strings.Count(got, `\"`)+2 {
		t.Errorf("an inner quote was not escaped: %q", got)
	}
}

func TestADatasetNameIsValidatedAtTheEdge(t *testing.T) {
	for _, bad := range []string{"", "retail`.`other", "retail; DROP", "retail-prod", "retail.public", "retail "} {
		if err := ValidateDatasetID(bad); err == nil {
			t.Errorf("%q was accepted as a dataset name", bad)
		}
	}
	if err := ValidateDatasetID("retail_prod_2"); err != nil {
		t.Errorf("a plain identifier was rejected: %v", err)
	}
}

func containing(notes []string, substring string) bool {
	for _, n := range notes {
		if strings.Contains(n, substring) {
			return true
		}
	}
	return false
}

// TestAStructIsLeftOutRatherThanCalledAString.
//
// A RECORD or a repeated field is not a dimension. Emitting it as a String
// would produce a model that looks complete, groups by a value no filter can
// match, and contradicts the warehouse the next time `doctor` runs.
func TestAStructIsLeftOutRatherThanCalledAString(t *testing.T) {
	md := &bigquery.TableMetadata{
		Schema: bigquery.Schema{
			{Name: "order_id", Type: bigquery.StringFieldType},
			{Name: "address", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{
				{Name: "city", Type: bigquery.StringFieldType},
			}},
			{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		},
	}

	got, skipped := describeTable("orders", md)

	if len(got.Columns) != 1 || got.Columns[0].Name != "order_id" {
		t.Errorf("want only the scalar column, got %+v", got.Columns)
	}
	if len(skipped) != 2 {
		t.Errorf("want both the struct and the array named, got %v", skipped)
	}
}
