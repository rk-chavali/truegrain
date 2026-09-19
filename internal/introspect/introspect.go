// Package introspect reads a warehouse's own metadata into the shape a
// semantic model needs.
//
// It exists because the honest answer to "how do I try this on my data" was
// "hand-write Ossie YAML", which is the cold start every semantic layer hits
// and the reason most of them are never tried twice.
//
// What it reads is deliberately narrow: tables, columns, types, primary keys
// and foreign keys. No rows, ever. That keeps the credential it needs down to
// permission to list a schema, which is a credential somebody will actually
// give a tool they are evaluating.
//
// The keys are the part that matters, and the part easy to skip. Columns alone
// produce a model that compiles and cannot detect a fan-out, because grain is
// derived from the primary key and joins are derived from the foreign keys.
// A model without them looks complete and silently inflates sums, which is the
// exact failure this project exists to prevent.
package introspect

import (
	"fmt"
	"sort"
	"strings"
)

// Column is one field as the warehouse describes it.
type Column struct {
	Name string
	// Type is the warehouse's own spelling, kept verbatim so a generated model
	// can be checked back against the warehouse without a lossy round trip.
	Type     string
	Nullable bool
	// Description is whatever the warehouse carries, which is often the only
	// documentation that exists.
	Description string
}

// Table is one relation.
type Table struct {
	Name        string
	Description string
	Columns     []Column
	// PrimaryKey is what grain is derived from. Empty is a finding rather than
	// a detail: without it nothing can tell whether a join repeats rows.
	PrimaryKey []string
}

// Relationship is a declared foreign key, which becomes a join the planner can
// reason about.
//
// FromColumns and ToColumns are positional: the first of one pairs with the
// first of the other. A compound key paired the wrong way round produces a join
// that runs and returns the wrong rows, so this order is load-bearing and every
// reader must preserve it.
type Relationship struct {
	Name        string
	From        string
	FromColumns []string
	To          string
	ToColumns   []string
}

// Metric is a measure a source already defined.
//
// Only an importer produces these. `truegrain init` never does, and that is
// the deliberate difference between the two: a warehouse schema can say an
// order has a total, and only a person can say which total the business
// calls revenue. A semantic layer being imported from has already had that
// conversation, so discarding its answer would make somebody have it twice.
type Metric struct {
	Name        string
	Description string
	// Expression is ANSI SQL over the datasets in this schema.
	Expression string
	// Datatype is the Ossie spelling: Number, Integer, Decimal.
	Datatype string
}

// Schema is everything read from one dataset.
type Schema struct {
	// Name is the dataset or schema this came from.
	Name          string
	Tables        []Table
	Relationships []Relationship
	// Metrics is non-empty only when imported from something that declared
	// them. MetricsFile renders these when present and the write-your-own
	// template when not.
	Metrics []Metric
	// Notes are things the operator should know before trusting the result,
	// such as a table with no primary key.
	Notes []string
}

// TablesWithoutKeys names the tables that cannot have a grain derived.
//
// Reported rather than worked around. A guessed key is worse than none: the
// engine would believe it knows the grain, pass the fan-out check, and inflate
// a sum with complete confidence.
func (s Schema) TablesWithoutKeys() []string {
	var out []string
	for _, t := range s.Tables {
		if len(t.PrimaryKey) == 0 {
			out = append(out, t.Name)
		}
	}
	return out
}

// Finish sorts a schema and says what is missing from it.
//
// Shared by every reader and by the importers. Sorting is what makes running
// init twice produce the same file, so regenerating after a schema change is
// a readable diff rather than a reshuffle. Exported so an importer goes
// through the same pass: an imported model with no grain has to be reported
// in the same words as a read one, or somebody learns the warning only from
// whichever source happened to produce it.
func Finish(s Schema) Schema {
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	sort.Slice(s.Relationships, func(i, j int) bool {
		if s.Relationships[i].From != s.Relationships[j].From {
			return s.Relationships[i].From < s.Relationships[j].From
		}
		if s.Relationships[i].To != s.Relationships[j].To {
			return s.Relationships[i].To < s.Relationships[j].To
		}
		return s.Relationships[i].Name < s.Relationships[j].Name
	})

	for _, missing := range s.TablesWithoutKeys() {
		s.Notes = append(s.Notes, fmt.Sprintf(
			"%s declares no primary key, so its grain cannot be derived and a fan-out "+
				"through it cannot be detected", missing))
	}
	if len(s.Relationships) == 0 && len(s.Tables) > 1 {
		s.Notes = append(s.Notes,
			"no foreign keys are declared, so nothing can be joined. Declare them, "+
				"or write relationships by hand in the generated model.")
	}
	return s
}

// relationshipName produces something a person would have written.
//
// An auto-generated constraint name is often no name at all, and a name that
// starts with a warehouse's own punctuation reads badly in a model file.
func relationshipName(constraint, from, to string) string {
	name := strings.TrimSpace(constraint)
	if _, rest, ok := strings.Cut(name, "."); ok {
		name = rest
	}
	if name == "" || strings.HasPrefix(name, "$") {
		return from + "_to_" + to
	}
	return name
}
