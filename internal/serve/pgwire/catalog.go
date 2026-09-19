package pgwire

import (
	"strings"
)

// The questions a client asks before it will ask a real one.
//
// psql sends a version check and a handful of settings on connect. A driver
// sends SET and SHOW. A BI tool lists tables and columns before it will draw
// anything. None of these are semantic questions, and none of them can be
// answered by the planner, so they are answered here from what the model
// already knows.
//
// This file holds the statements that have no relational shape at all: the
// session chatter, and the two psql metacommands whose SQL is a join across
// half a dozen catalogue tables. Everything that is a SELECT over a
// catalogue table goes to the restricted evaluator in catalog_select.go,
// which reads rows out of the synthesised relations in catalog_relations.go
// and applies the caller's own filter.
//
// This is deliberately not a synthesised pg_catalog. A real pg_catalog is a
// large surface that invites a client to ask anything at all about a
// database that does not exist, and every answer would be a fiction that
// some tool eventually depends on. Answering the relations clients actually
// read, correctly and including their filters, and refusing the rest with a
// message naming what does exist, is smaller and more honest.

// result is a small fixed answer: a catalogue reply rather than a query
// result. It carries its own types because nothing inferred them from rows.
type result struct {
	columns []string
	oids    []uint32
	rows    [][]any
	// tag is what CommandComplete reports. SELECT needs a row count; SET and
	// others have their own word and no count.
	tag string
	// exact marks an answer whose shape is already the one asked for. Every
	// answer built here is: the evaluator projects to the caller's select
	// list, and the fixed replies below are single-column by construction.
	// Handing back a shape the client did not ask for is a protocol error on
	// its side rather than a cosmetic difference: psql reports "column
	// number 8 is out of range" and pgx refuses the result outright.
	exact bool
}

// catalogAnswer handles a statement that is not a semantic query.
//
// The second return distinguishes "handled" from "not mine", so the caller
// can fall through to the semantic parser. An unrecognised statement is not
// an error here.
func catalogAnswer(sql string, schemas []namespaceSchema, serverVersion string) (*result, bool) {
	normalized := normalize(sql)

	switch {
	// Settings a driver pushes at connect time. Accepted and discarded: this
	// server has one behaviour and no session state to change, and failing
	// them would stop most clients before their first query.
	case strings.HasPrefix(normalized, "set "):
		return &result{tag: "SET"}, true
	case strings.HasPrefix(normalized, "begin"), strings.HasPrefix(normalized, "start transaction"):
		// Every query here is a single read. A transaction is accepted so a
		// driver's autocommit wrapper works, and means nothing, which is true
		// rather than convenient: there is no write to isolate.
		return &result{tag: "BEGIN"}, true
	case strings.HasPrefix(normalized, "commit"):
		return &result{tag: "COMMIT"}, true
	case strings.HasPrefix(normalized, "rollback"):
		return &result{tag: "ROLLBACK"}, true
	case normalized == "", normalized == ";":
		return &result{tag: "EMPTY"}, true

	case strings.HasPrefix(normalized, "show "):
		name := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(normalized, "show ")), ";")
		return &result{
			columns: []string{name},
			oids:    []uint32{oidText},
			rows:    [][]any{{settings(serverVersion)[name]}},
			tag:     "SHOW", exact: true,
		}, true

	case strings.Contains(normalized, "version()"):
		return &result{
			columns: []string{"version"},
			oids:    []uint32{oidText},
			rows:    [][]any{{versionString(serverVersion)}},
			tag:     "SELECT", exact: true,
		}, true

	case strings.Contains(normalized, "current_schema"):
		return &result{
			columns: []string{"current_schema"},
			oids:    []uint32{oidText},
			rows:    [][]any{{"public"}},
			tag:     "SELECT", exact: true,
		}, true

	// psql's own metacommands, recognised by fingerprint.
	//
	// \dt and \d send joins over half a dozen catalogue tables with CASE
	// expressions and regex operators, which nothing short of a real SQL
	// engine parses. They are matched on what makes them distinctive and
	// answered with the exact columns psql binds, because the first thing a
	// person does after connecting is type \dt and an error there reads as
	// the whole thing being broken.
	//
	// Pattern matched, and the pattern will eventually go stale when psql
	// changes its queries. That is an acceptable failure: it degrades to the
	// refusal below, which names what does work, rather than to a wrong
	// answer.
	case isPsqlRelationList(normalized):
		return psqlRelationList(schemas), true
	}

	// Everything else that could be a catalogue question goes through the
	// restricted evaluator, which applies the caller's own filter.
	//
	// It declines anything it does not fully understand, and declining is
	// the point. The version this replaced matched on a statement merely
	// mentioning pg_class and answered with the whole table listing, so a
	// client narrowing to one table with a WHERE clause got every table and
	// no indication that its filter had been dropped. Rows a caller asked to
	// exclude are worse than no rows at all: a refusal is visible and a
	// field picker full of the wrong fields is not.
	return evalCatalogSelect(sql, schemas)
}

// isCatalogProbe reports whether an unhandled statement was asking about the
// catalogue, so the refusal can say so rather than complain about grammar.
func isCatalogProbe(sql string) bool {
	n := normalize(sql)
	return strings.Contains(n, "pg_catalog.") || strings.Contains(n, "information_schema.") ||
		strings.Contains(n, "pg_")
}

// normalize lowercases and collapses whitespace, so a statement split across
// lines by a driver matches the same prefix as one on a single line.
func normalize(sql string) string {
	return strings.ToLower(strings.Join(strings.Fields(sql), " "))
}

func settings(serverVersion string) map[string]string {
	return map[string]string{
		"server_version":              serverVersion,
		"server_encoding":             "UTF8",
		"client_encoding":             "UTF8",
		"datestyle":                   "ISO, MDY",
		"timezone":                    "UTC",
		"standard_conforming_strings": "on",
		"transaction_isolation":       "read committed",
		"transaction_read_only":       "on",
		"search_path":                 "public",
		"application_name":            "",
		"integer_datetimes":           "on",
	}
}

func versionString(serverVersion string) string {
	// Names itself. A client that logs what it connected to should not report
	// a PostgreSQL that is not there, and anybody debugging a wrong number
	// needs to know a semantic layer was in the path.
	return "PostgreSQL " + serverVersion + " (truegrain semantic engine)"
}

// trimNamespace drops the namespace prefix, because the namespace is already
// the table name and repeating it would make every column read
// retail.retail.customers.region in a field picker.
func trimNamespace(name, namespace string) string {
	if rest, ok := cutPrefixFold(name, namespace+"."); ok {
		return rest
	}
	return name
}

// isPsqlRelationList recognises the query behind psql's \dt and \dv.
//
// The fingerprint is the combination: it selects from pg_class joined to
// pg_namespace and filters on relkind, which together no BI tool's simpler
// table listing does.
func isPsqlRelationList(normalized string) bool {
	return strings.Contains(normalized, "pg_catalog.pg_class") &&
		strings.Contains(normalized, "c.relkind") &&
		strings.Contains(normalized, "pg_table_is_visible")
}

// psqlRelationList answers with the four columns psql's \dt binds, in its
// order and under its headings.
func psqlRelationList(schemas []namespaceSchema) *result {
	r := &result{
		columns: []string{"Schema", "Name", "Type", "Owner"},
		oids:    []uint32{oidText, oidText, oidText, oidText},
		tag:     "SELECT",
		// Exact, because psql binds these four and narrowing by the select
		// list is impossible: it is full of CASE expressions.
		exact: true,
	}
	for _, s := range schemas {
		// "table" rather than "view", because a namespace is the thing a
		// caller selects from and calling it a view would suggest it has a
		// definition to read.
		r.rows = append(r.rows, []any{"public", s.name, "table", "truegrain"})
	}
	return r
}
