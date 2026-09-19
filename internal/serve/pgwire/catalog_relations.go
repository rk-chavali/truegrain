package pgwire

import (
	"strings"
)

// The catalogue as relations rather than as answers.
//
// The first version of this file was a switch: a statement that mentioned
// pg_class got a table listing, one that mentioned pg_attribute got a column
// listing, and the select list was narrowed to the names the caller asked
// for. That is fine until a client sends a WHERE clause, and then it is
// worse than a refusal, because the filter is silently dropped and the
// caller gets every row of a listing they asked to restrict. A BI tool
// asking for the columns of one table and being handed the columns of all of
// them does not fail: it draws a field picker with the wrong fields in it.
//
// So the synthesised catalogue is defined here as rows, and the small
// evaluator in catalog_select.go runs a restricted SELECT over them. It
// understands projection, a conjunction of simple predicates, ORDER BY and
// LIMIT, and it declines anything else rather than approximating it.
// Declining falls through to a refusal that names what does work.
//
// This is still not pg_catalog and is not trying to be. A join across
// catalogue tables, a CASE expression, a cast to regclass, ANY() over an
// array: all of those are ordinary in what psql and Tableau send, and all of
// them are declined here. The two things that changed are that a statement
// this does answer is now answered correctly including its filter, and that
// a statement it does not answer is logged with what the client called
// itself, so an operator finds out which tool sent what instead of hearing
// that the connection "does not work".
//
// Relations whose honest answer is no rows at all are present and empty.
// A namespace is not a view and has no indexes, so a client listing views
// gets an empty set, which is true, rather than a refusal, which reads as a
// broken server.

// firstUserOID is where PostgreSQL's own user-object OIDs start. Synthesised
// OIDs begin here so they cannot collide with a built-in one a client has
// hardcoded, and so that a person reading a trace recognises them as objects
// rather than as catalogue entries.
const firstUserOID = 16384

// publicNamespaceOID is the single schema this server presents. 2200 is the
// OID PostgreSQL gives to public, and clients do hardcode it.
const publicNamespaceOID = 2200

// relation is one synthesised catalogue table.
type relation struct {
	// names are every spelling a client may use, lowercased. A relation is
	// reachable both qualified and bare because drivers differ.
	names   []string
	columns []string
	oids    []uint32
	rows    func([]namespaceSchema) [][]any
}

// catalogRelations returns the synthesised catalogue.
//
// Built per call rather than kept in a package variable: the row functions
// close over nothing, but returning a fresh slice means no caller can hold a
// reference and mutate the registry that every connection reads.
func catalogRelations() []relation {
	return []relation{
		{
			names:   []string{"pg_namespace", "pg_catalog.pg_namespace"},
			columns: []string{"oid", "nspname", "nspowner"},
			oids:    []uint32{oidInt8, oidText, oidInt8},
			rows: func([]namespaceSchema) [][]any {
				return [][]any{{int64(publicNamespaceOID), "public", int64(10)}}
			},
		},
		{
			names:   []string{"information_schema.schemata"},
			columns: []string{"catalog_name", "schema_name", "schema_owner"},
			oids:    []uint32{oidText, oidText, oidText},
			rows: func([]namespaceSchema) [][]any {
				return [][]any{{"truegrain", "public", "truegrain"}}
			},
		},
		{
			// Each namespace is one table. That is the shape the semantic
			// grammar accepts, so a tool that lists here and then selects
			// writes a statement this server can actually answer.
			names: []string{"pg_class", "pg_catalog.pg_class"},
			columns: []string{"oid", "relname", "relnamespace", "relkind", "relowner",
				"reltuples", "relhasindex", "relispartition", "relpersistence"},
			oids: []uint32{oidInt8, oidText, oidInt8, oidText, oidInt8,
				oidFloat8, oidBool, oidBool, oidText},
			rows: func(schemas []namespaceSchema) [][]any {
				var out [][]any
				for i, s := range schemas {
					out = append(out, []any{
						int64(firstUserOID + i), s.name, int64(publicNamespaceOID),
						// 'r' for an ordinary table. Calling it 'v' would
						// promise a definition somebody could read.
						"r", int64(10),
						// -1 is PostgreSQL's "never analysed", which is the
						// truth: there is no row count until the warehouse
						// is asked, and inventing one would mislead a
						// planner-aware client.
						float64(-1), false, false, "p",
					})
				}
				return out
			},
		},
		{
			names: []string{"pg_attribute", "pg_catalog.pg_attribute"},
			columns: []string{"attrelid", "attname", "attnum", "atttypid", "attnotnull",
				"atthasdef", "attisdropped", "attlen", "atttypmod"},
			oids: []uint32{oidInt8, oidText, oidInt8, oidInt8, oidBool,
				oidBool, oidBool, oidInt8, oidInt8},
			rows: func(schemas []namespaceSchema) [][]any {
				var out [][]any
				for i, s := range schemas {
					for position, f := range fieldsOf(s) {
						out = append(out, []any{
							int64(firstUserOID + i), f.name, int64(position + 1),
							int64(f.typeOID), false, false, false, int64(-1), int64(-1),
						})
					}
				}
				return out
			},
		},
		{
			// The types this server can actually return, and no others.
			// A client that looks up an OID not in this list learns that
			// nothing here produces that type, which is true.
			names:   []string{"pg_type", "pg_catalog.pg_type"},
			columns: []string{"oid", "typname", "typnamespace", "typtype", "typlen"},
			oids:    []uint32{oidInt8, oidText, oidInt8, oidText, oidInt8},
			rows: func([]namespaceSchema) [][]any {
				var out [][]any
				for _, t := range []struct {
					oid    uint32
					name   string
					length int64
				}{
					{oidBool, "bool", 1},
					{oidInt8, "int8", 8},
					{oidFloat8, "float8", 8},
					{oidText, "text", -1},
					{oidNumeric, "numeric", -1},
					{oidDate, "date", 4},
					{oidTimestamp, "timestamp", 8},
					{oidTimestamptz, "timestamptz", 8},
				} {
					out = append(out, []any{
						int64(t.oid), t.name, int64(11), "b", t.length,
					})
				}
				return out
			},
		},
		{
			names: []string{"pg_tables", "pg_catalog.pg_tables"},
			columns: []string{"schemaname", "tablename", "tableowner", "tablespace",
				"hasindexes", "hasrules", "hastriggers", "rowsecurity"},
			oids: []uint32{oidText, oidText, oidText, oidText,
				oidBool, oidBool, oidBool, oidBool},
			rows: func(schemas []namespaceSchema) [][]any {
				var out [][]any
				for _, s := range schemas {
					out = append(out, []any{
						"public", s.name, "truegrain", nil, false, false, false, false,
					})
				}
				return out
			},
		},
		{
			names:   []string{"information_schema.tables"},
			columns: []string{"table_catalog", "table_schema", "table_name", "table_type"},
			oids:    []uint32{oidText, oidText, oidText, oidText},
			rows: func(schemas []namespaceSchema) [][]any {
				var out [][]any
				for _, s := range schemas {
					out = append(out, []any{"truegrain", "public", s.name, "BASE TABLE"})
				}
				return out
			},
		},
		{
			// A metric is numeric and a dimension is text. That distinction
			// is the whole reason a BI tool reads this relation: reporting a
			// measure as text is what makes it show up as a category and get
			// dropped on the wrong shelf.
			names: []string{"information_schema.columns"},
			columns: []string{"table_catalog", "table_schema", "table_name", "column_name",
				"ordinal_position", "data_type", "is_nullable", "udt_name"},
			oids: []uint32{oidText, oidText, oidText, oidText,
				oidInt8, oidText, oidText, oidText},
			rows: func(schemas []namespaceSchema) [][]any {
				var out [][]any
				for _, s := range schemas {
					for position, f := range fieldsOf(s) {
						out = append(out, []any{
							"truegrain", "public", s.name, f.name,
							int64(position + 1), f.sqlType, "YES", f.sqlType,
						})
					}
				}
				return out
			},
		},

		// Empty by construction, and present so that asking is not an error.
		// A namespace is a governed set of metrics and dimensions. It has no
		// view definition, no index and no sequence, so the honest answer to
		// each of these is no rows.
		emptyRelation([]string{"pg_views", "pg_catalog.pg_views"},
			"schemaname", "viewname", "viewowner", "definition"),
		emptyRelation([]string{"information_schema.views"},
			"table_catalog", "table_schema", "table_name", "view_definition"),
		emptyRelation([]string{"pg_indexes", "pg_catalog.pg_indexes"},
			"schemaname", "tablename", "indexname", "tablespace", "indexdef"),
		emptyRelation([]string{"pg_matviews", "pg_catalog.pg_matviews"},
			"schemaname", "matviewname", "matviewowner", "definition"),
	}
}

// emptyRelation is a relation that exists and has no rows, with every column
// typed text because nothing will ever be read out of it.
func emptyRelation(names []string, columns ...string) relation {
	oids := make([]uint32, len(columns))
	for i := range oids {
		oids[i] = oidText
	}
	return relation{
		names:   names,
		columns: columns,
		oids:    oids,
		rows:    func([]namespaceSchema) [][]any { return nil },
	}
}

// catalogField is one column of a synthesised table: a dimension or a metric.
type catalogField struct {
	name    string
	sqlType string
	typeOID uint32
}

// fieldsOf lists a namespace's columns in the order a client should see
// them, dimensions first. The ordinal position a client reads comes from
// this order, so it has to be one order rather than whatever a map iteration
// produced.
func fieldsOf(s namespaceSchema) []catalogField {
	out := make([]catalogField, 0, len(s.dimensions)+len(s.metrics))
	for _, d := range s.dimensions {
		out = append(out, catalogField{trimNamespace(d, s.name), "text", oidText})
	}
	for _, m := range s.metrics {
		out = append(out, catalogField{trimNamespace(m, s.name), "numeric", oidNumeric})
	}
	return out
}

// findRelation resolves a name a client wrote to a synthesised relation.
func findRelation(name string) (relation, bool) {
	name = strings.ToLower(name)
	for _, r := range catalogRelations() {
		for _, candidate := range r.names {
			if candidate == name {
				return r, true
			}
		}
	}
	return relation{}, false
}
