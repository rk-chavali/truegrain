package introspect

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Reading a PostgreSQL schema's own description of itself.
//
// Through the pg_catalog tables rather than information_schema, for the same
// reason the BigQuery reader avoids INFORMATION_SCHEMA: the standard views
// spread one constraint across several rows and record no reliable way to pair
// the two sides of a compound key. The catalogue keeps both sides as parallel
// arrays on a single row of pg_constraint, conkey and confkey, where position
// i of one pairs with position i of the other by construction.
//
// That pairing is load-bearing. The BigQuery reader shipped a bug where a
// two-column key came back as four columns, because it joined KEY_COLUMN_USAGE
// to CONSTRAINT_COLUMN_USAGE on the constraint name: a fan-out inside the
// fan-out detector, and every unit test passed. unnest(conkey, confkey) WITH
// ORDINALITY cannot reproduce it, because the pairing is a property of the
// data rather than something this code reconstructs.
//
// Every query is parameterised on the schema name and reads only the
// catalogue. No rows are read from any user table, ever, so the credential
// this needs is CONNECT plus USAGE on the schema. That is a credential
// somebody will hand to a tool they are still evaluating.
//
// ponytail: four round trips rather than one join, because four readable
// queries beat one clever one and this is a command a person runs once.

// tableKinds are the relations worth modelling: ordinary and partitioned
// tables, views, materialised views and foreign tables.
//
// Views are included deliberately, matching BigQuery. A view is a perfectly
// good thing to model and excluding them would silently drop half of most
// warehouses. A view carries no constraints, so it arrives without a key and
// is reported as such rather than guessed at.
const tableKinds = `('r', 'p', 'v', 'm', 'f')`

// Postgres reads one schema.
//
// The DSN arrives from the environment via the caller, never from a flag: a
// connection string on a command line lands in shell history and in the
// process list, and a password is the one value that must be in neither.
func Postgres(ctx context.Context, dsn, schema string) (Schema, error) {
	if err := ValidateSchemaName(schema); err != nil {
		return Schema{}, err
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		// pgx redacts the password in its own errors. Nothing here should put
		// the DSN into a message by hand and undo that.
		return Schema{}, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Read-only for the same reason the executor is: this only ever selects
	// from the catalogue, and a server-enforced guarantee of that beats a
	// convention. It also gives every query one consistent snapshot, so a
	// migration running alongside cannot produce a schema with a foreign key
	// to a table that is missing from the table list.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Schema{}, fmt.Errorf("starting a read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out := Schema{Name: schema}

	tables, order, err := readTables(ctx, tx, schema)
	if err != nil {
		return Schema{}, err
	}
	if len(order) == 0 {
		return Schema{}, fmt.Errorf(
			"schema %q has no tables, or this credential cannot see them", schema)
	}

	skipped, composites, err := readColumns(ctx, tx, schema, tables)
	if err != nil {
		return Schema{}, err
	}
	if err := readPrimaryKeys(ctx, tx, schema, tables); err != nil {
		return Schema{}, err
	}
	if out.Relationships, err = readForeignKeys(ctx, tx, schema); err != nil {
		return Schema{}, err
	}

	for _, name := range order {
		out.Tables = append(out.Tables, *tables[name])
	}
	if len(skipped) > 0 {
		note := fmt.Sprintf(
			"left out %s, because an array or a composite type is not a dimension",
			strings.Join(skipped, ", "))
		// The (x).field syntax only addresses a composite. Suggesting it for
		// an array would send somebody to write an expression that does not
		// parse, so the example appears only when there is a composite to
		// point at.
		if len(composites) > 0 {
			note += fmt.Sprintf(". Address a leaf by hand, for example expression: (%s).city",
				composites[0])
		}
		out.Notes = append(out.Notes, note)
	}
	return Finish(out), nil
}

// readTables lists the relations and their comments.
//
// The returned order preserves query order so the caller does not depend on
// map iteration. finish sorts the result anyway, but a reader that is
// deterministic before sorting is easier to debug.
func readTables(ctx context.Context, tx pgx.Tx, schema string) (map[string]*Table, []string, error) {
	const q = `
		SELECT c.relname, COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ` + tableKinds + `
		ORDER BY c.relname`

	rows, err := tx.Query(ctx, q, schema)
	if err != nil {
		return nil, nil, fmt.Errorf("listing tables in schema %q: %w", schema, err)
	}
	defer rows.Close()

	tables := map[string]*Table{}
	var order []string
	for rows.Next() {
		var name, description string
		if err := rows.Scan(&name, &description); err != nil {
			return nil, nil, fmt.Errorf("reading the table list: %w", err)
		}
		tables[name] = &Table{Name: name, Description: description}
		order = append(order, name)
	}
	return tables, order, rows.Err()
}

// readColumns fills in the fields, and reports the ones left out.
//
// An array or a composite column is skipped rather than emitted as a String.
// Emitting one would produce a model that looks complete, groups by a value no
// filter can match, and compares badly against the warehouse in doctor. Naming
// it and leaving it out is the honest shape, and it matches what the BigQuery
// reader does with a RECORD or a REPEATED field.
// The two skipped lists are returned separately because only a composite can
// be addressed with the (x).field syntax the note suggests.
func readColumns(ctx context.Context, tx pgx.Tx, schema string, tables map[string]*Table) (skipped, composites []string, err error) {
	const q = `
		SELECT c.relname,
		       a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(col_description(a.attrelid, a.attnum), ''),
		       t.typcategory = 'A',
		       t.typtype = 'c'
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE n.nspname = $1 AND c.relkind IN ` + tableKinds + `
		  AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum`

	rows, err := tx.Query(ctx, q, schema)
	if err != nil {
		return nil, nil, fmt.Errorf("reading columns in schema %q: %w", schema, err)
	}
	defer rows.Close()

	for rows.Next() {
		var table, name, dataType, description string
		var nullable, isArray, isComposite bool
		if err := rows.Scan(&table, &name, &dataType, &nullable, &description,
			&isArray, &isComposite); err != nil {
			return nil, nil, fmt.Errorf("reading a column: %w", err)
		}
		t, ok := tables[table]
		if !ok {
			continue
		}
		if isArray || isComposite {
			qualified := table + "." + name
			skipped = append(skipped, qualified)
			if isComposite {
				composites = append(composites, qualified)
			}
			continue
		}
		t.Columns = append(t.Columns, Column{
			Name:        name,
			Type:        dataType,
			Nullable:    nullable,
			Description: description,
		})
	}
	return skipped, composites, rows.Err()
}

// readPrimaryKeys sets the grain each table's fan-out check depends on.
//
// conkey is an ordered array of column numbers, and WITH ORDINALITY carries
// that order through the unnest. A key read out of order still detects a
// fan-out, but it would produce different bytes on each run, and init running
// twice producing the same file is what makes regenerating after a schema
// change a readable diff rather than a reshuffle.
func readPrimaryKeys(ctx context.Context, tx pgx.Tx, schema string, tables map[string]*Table) error {
	const q = `
		SELECT rel.relname, att.attname
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = rel.relnamespace
		CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = k.attnum
		WHERE con.contype = 'p' AND ns.nspname = $1
		ORDER BY rel.relname, k.ord`

	rows, err := tx.Query(ctx, q, schema)
	if err != nil {
		return fmt.Errorf("reading primary keys in schema %q: %w", schema, err)
	}
	defer rows.Close()

	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return fmt.Errorf("reading a primary key column: %w", err)
		}
		if t, ok := tables[table]; ok {
			t.PrimaryKey = append(t.PrimaryKey, column)
		}
	}
	return rows.Err()
}

// readForeignKeys turns declared foreign keys into joins the planner can use.
//
// unnest(con.conkey, con.confkey) walks both arrays in step, so the
// referencing column and the column it references arrive on the same row.
// This is the whole reason for reading pg_catalog: the pairing is a property
// of the data rather than something reconstructed by a join, which is where
// the equivalent BigQuery code went wrong.
//
// A key pointing at another schema is skipped. The model has no dataset to
// join to, and emitting a relationship naming one that does not exist fails
// validation later with a confusing message.
func readForeignKeys(ctx context.Context, tx pgx.Tx, schema string) ([]Relationship, error) {
	const q = `
		SELECT con.conname,
		       src.relname,
		       tgt.relname,
		       src_att.attname,
		       tgt_att.attname
		FROM pg_constraint con
		JOIN pg_class src ON src.oid = con.conrelid
		JOIN pg_class tgt ON tgt.oid = con.confrelid
		JOIN pg_namespace src_ns ON src_ns.oid = src.relnamespace
		JOIN pg_namespace tgt_ns ON tgt_ns.oid = tgt.relnamespace
		CROSS JOIN LATERAL unnest(con.conkey, con.confkey)
		     WITH ORDINALITY AS k(src_attnum, tgt_attnum, ord)
		JOIN pg_attribute src_att
		  ON src_att.attrelid = con.conrelid AND src_att.attnum = k.src_attnum
		JOIN pg_attribute tgt_att
		  ON tgt_att.attrelid = con.confrelid AND tgt_att.attnum = k.tgt_attnum
		WHERE con.contype = 'f'
		  AND src_ns.nspname = $1
		  AND tgt_ns.nspname = $1
		ORDER BY src.relname, tgt.relname, con.conname, k.ord`

	rows, err := tx.Query(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("reading foreign keys in schema %q: %w", schema, err)
	}
	defer rows.Close()

	// Rows arrive grouped by constraint and ordered within it, so appending in
	// arrival order is what preserves the pairing.
	var out []Relationship
	byConstraint := map[string]int{}
	for rows.Next() {
		var constraint, from, to, fromColumn, toColumn string
		if err := rows.Scan(&constraint, &from, &to, &fromColumn, &toColumn); err != nil {
			return nil, fmt.Errorf("reading a foreign key column: %w", err)
		}
		// Keyed on the table as well as the name: a constraint name is unique
		// per table in PostgreSQL, not per schema, so two tables may each
		// carry a constraint called fk_customer.
		key := from + "\x00" + constraint
		i, seen := byConstraint[key]
		if !seen {
			out = append(out, Relationship{
				Name: relationshipName(constraint, from, to),
				From: from,
				To:   to,
			})
			i = len(out) - 1
			byConstraint[key] = i
		}
		out[i].FromColumns = append(out[i].FromColumns, fromColumn)
		out[i].ToColumns = append(out[i].ToColumns, toColumn)
	}
	// Names are per table in PostgreSQL too, so the keying above prevents two
	// tables' constraints merging without preventing them colliding.
	return uniqueRelationshipNames(out), rows.Err()
}

// ValidateSchemaName refuses a name that cannot be a schema.
//
// The name is bound as a query parameter everywhere it is used, so nothing
// here is protecting against injection today. The check stays at the edge
// because it turns a typo into one clear sentence rather than into "schema has
// no tables", and because the next reader added may well interpolate.
func ValidateSchemaName(schema string) error {
	if strings.TrimSpace(schema) == "" {
		return fmt.Errorf("a schema is required, for example public")
	}
	if len(schema) > 63 {
		return fmt.Errorf(
			"%q is longer than the 63 characters PostgreSQL allows in a schema name",
			schema[:32]+"...")
	}
	for _, r := range schema {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("a schema name cannot contain control characters")
		}
	}
	return nil
}
