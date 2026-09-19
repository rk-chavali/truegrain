package introspect

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// Reading a BigQuery dataset's own description of itself.
//
// Through tables.list and tables.get, not INFORMATION_SCHEMA. The first version
// used two queries and was wrong twice over: joining KEY_COLUMN_USAGE to
// CONSTRAINT_COLUMN_USAGE on the constraint name multiplied a two-column key
// into four rows, and CONSTRAINT_COLUMN_USAGE records no ordinal position, so
// nothing said which column of a compound foreign key paired with which.
//
// The API answers both exactly: a primary key arrives as an ordered list, and a
// foreign key arrives as explicit referencing/referenced column pairs. It also
// costs nothing and runs no query job, so the credential is
// roles/bigquery.metadataViewer and there is no SQL to be injected into. That
// is a credential somebody will hand to a tool they are still deciding about.
//
// ponytail: one tables.get per table, so a dataset with thousands of tables is
// thousands of calls. Fine for a command a person runs once on one dataset; if
// that stops being true, INFORMATION_SCHEMA.COLUMNS is the bulk read, and the
// constraints still have to come from here.

// BigQuery reads one dataset.
func BigQuery(ctx context.Context, project, dataset string) (Schema, error) {
	if err := ValidateDatasetID(dataset); err != nil {
		return Schema{}, err
	}
	client, err := bigquery.NewClient(ctx, project)
	if err != nil {
		return Schema{}, fmt.Errorf("connecting to BigQuery project %s: %w", project, err)
	}
	defer client.Close()

	out := Schema{Name: dataset}
	qualified := project + "." + dataset
	var nested []string

	it := client.Dataset(dataset).Tables(ctx)
	for {
		table, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return Schema{}, fmt.Errorf("listing tables in %s: %w", qualified, err)
		}

		md, err := table.Metadata(ctx)
		if err != nil {
			return Schema{}, fmt.Errorf("reading %s.%s: %w", qualified, table.TableID, err)
		}

		described, skipped := describeTable(table.TableID, md)
		out.Tables = append(out.Tables, described)
		out.Relationships = append(out.Relationships,
			relationshipsOf(table.TableID, dataset, md)...)
		nested = append(nested, skipped...)
	}

	if len(nested) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"left out %s, because a struct or an array is not a dimension. "+
				"Address a leaf by hand, for example expression: %s.city",
			strings.Join(nested, ", "), nested[0]))
	}

	if len(out.Tables) == 0 {
		return Schema{}, fmt.Errorf(
			"dataset %s has no tables, or this credential cannot see them", qualified)
	}
	return Finish(out), nil
}

// describeTable turns one table's metadata into a dataset definition.
//
// Views are included: a view is a perfectly good thing to model, and excluding
// them would silently drop half of most warehouses. A view carries no
// constraints, so it arrives without a key and is reported as such.
func describeTable(name string, md *bigquery.TableMetadata) (Table, []string) {
	t := Table{Name: name, Description: md.Description}

	var skipped []string
	for _, f := range md.Schema {
		// A struct or an array is not a dimension. Emitting one as a String
		// field would produce a model that looks complete, groups by a value
		// no filter can match, and compares badly against the warehouse in
		// `doctor`. Naming it and leaving it out is the honest shape.
		if f.Type == bigquery.RecordFieldType || f.Repeated {
			skipped = append(skipped, name+"."+f.Name)
			continue
		}
		t.Columns = append(t.Columns, Column{
			Name:        f.Name,
			Type:        string(f.Type),
			Nullable:    !f.Required,
			Description: f.Description,
		})
	}

	if md.TableConstraints != nil && md.TableConstraints.PrimaryKey != nil {
		t.PrimaryKey = md.TableConstraints.PrimaryKey.Columns
	}
	return t, skipped
}

// relationshipsOf turns a table's foreign keys into joins.
//
// A key pointing outside the dataset being read is skipped rather than emitted,
// because the model has no dataset to join to and a relationship naming one
// that does not exist fails validation with a confusing message.
func relationshipsOf(name, dataset string, md *bigquery.TableMetadata) []Relationship {
	if md.TableConstraints == nil {
		return nil
	}

	var out []Relationship
	for _, fk := range md.TableConstraints.ForeignKeys {
		if fk.ReferencedTable == nil || fk.ReferencedTable.DatasetID != dataset {
			continue
		}
		to := fk.ReferencedTable.TableID
		rel := Relationship{
			Name: relationshipName(fk.Name, name, to),
			From: name,
			To:   to,
		}
		// The API pairs the columns explicitly, which is the whole reason for
		// reading it rather than the information schema. Appending both sides
		// in step is what keeps that pairing true.
		for _, c := range fk.ColumnReferences {
			rel.FromColumns = append(rel.FromColumns, c.ReferencingColumn)
			rel.ToColumns = append(rel.ToColumns, c.ReferencedColumn)
		}
		if len(rel.FromColumns) == 0 {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// ValidateDatasetID refuses anything that is not a plain identifier.
//
// Google's own rule is letters, digits and underscores. Nothing here builds SQL
// from the value any more, but the check stays at the edge: it turns a typo
// into one clear sentence instead of an API error about a resource nobody
// meant to name, and the next warehouse to be added may well interpolate.
func ValidateDatasetID(dataset string) error {
	if dataset == "" {
		return fmt.Errorf("a dataset is required")
	}
	for _, r := range dataset {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf(
				"%q is not a BigQuery dataset name; they contain only letters, digits and underscores",
				strings.TrimSpace(dataset))
		}
	}
	return nil
}
