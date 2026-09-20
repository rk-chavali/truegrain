package introspect

import "testing"

// Relationship names have to be unique per namespace, and neither warehouse
// makes constraint names unique per schema.
//
// Found by reading a real BigQuery dataset rather than a fixture. Two tables
// each carrying one unnamed foreign key both arrived as fk$1, and the model
// init wrote did not load. Nobody names foreign key constraints, so this was
// the ordinary case rather than the awkward one.

func TestAWarehouseGeneratedNameIsReplaced(t *testing.T) {
	for _, c := range []struct {
		constraint, from, to, want string
	}{
		// BigQuery numbers unnamed keys per table, so the punctuation is in
		// the middle. Testing the prefix let these through.
		{"fk$1", "orders", "customers", "orders_to_customers"},
		{"fk$2", "order_lines", "products", "order_lines_to_products"},
		{"", "a", "b", "a_to_b"},
		{"$weird", "a", "b", "a_to_b"},
		// A name somebody chose is kept, because they chose it.
		{"orders_ship_to", "orders", "addresses", "orders_ship_to"},
		// PostgreSQL qualifies with the schema; only the last part is a name.
		{"public.fk_customer", "orders", "customers", "fk_customer"},
	} {
		if got := relationshipName(c.constraint, c.from, c.to); got != c.want {
			t.Errorf("relationshipName(%q, %q, %q) = %q, want %q",
				c.constraint, c.from, c.to, got, c.want)
		}
	}
}

func TestTwoTablesWithUnnamedKeysDoNotCollide(t *testing.T) {
	// Exactly what the demo dataset produced: every table's first foreign key
	// is fk$1, so the derived names are what keep them apart.
	got := uniqueRelationshipNames([]Relationship{
		{Name: "order_lines_to_orders", From: "order_lines", To: "orders", FromColumns: []string{"order_id"}},
		{Name: "orders_to_customers", From: "orders", To: "customers", FromColumns: []string{"customer_id"}},
	})

	if got[0].Name == got[1].Name {
		t.Fatalf("two relationships share a name: %+v", got)
	}
}

// TestOneTablePointingTwiceAtAnotherIsDisambiguated.
//
// The case the derived name cannot settle on its own, and an ordinary one: a
// shipping address and a billing address on the same order both point at
// addresses, so from_to_to is identical for both.
func TestOneTablePointingTwiceAtAnotherIsDisambiguated(t *testing.T) {
	got := uniqueRelationshipNames([]Relationship{
		{Name: "orders_to_addresses", From: "orders", To: "addresses", FromColumns: []string{"ship_to_id"}},
		{Name: "orders_to_addresses", From: "orders", To: "addresses", FromColumns: []string{"bill_to_id"}},
	})

	if got[0].Name == got[1].Name {
		t.Fatalf("two joins onto the same table share a name: %+v", got)
	}
	// Disambiguated by the column, because that is the thing that differs and
	// a reader comparing the two wants to see it without opening the warehouse.
	if got[1].Name != "orders_to_addresses_on_bill_to_id" {
		t.Errorf("want the column in the name, got %q", got[1].Name)
	}
	// The first keeps the name it had: renaming both would churn every model
	// that already referred to one of them.
	if got[0].Name != "orders_to_addresses" {
		t.Errorf("the first relationship was renamed to %q", got[0].Name)
	}
}

// TestIdenticalJoinsDeclaredTwiceStillGetSeparateNames, because there is
// nothing left to tell them apart by and returning a duplicate would fail
// validation with a message about the model rather than about the warehouse.
func TestIdenticalJoinsDeclaredTwiceStillGetSeparateNames(t *testing.T) {
	got := uniqueRelationshipNames([]Relationship{
		{Name: "a_to_b", From: "a", To: "b", FromColumns: []string{"x"}},
		{Name: "a_to_b", From: "a", To: "b", FromColumns: []string{"x"}},
		{Name: "a_to_b", From: "a", To: "b", FromColumns: []string{"x"}},
	})

	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.Name] {
			t.Fatalf("duplicate name %q survived: %+v", r.Name, got)
		}
		seen[r.Name] = true
	}
}
