package pgwire

import (
	"fmt"
	"math/big"
	"strconv"
	"time"
)

// Describing a semantic result in PostgreSQL's type vocabulary.
//
// The type matters more than it looks. A client told a column is text will
// render 885.50 as a label and refuse to sum it, so a dashboard built on this
// would show every measure as a category and no chart would plot. Getting the
// OID right is the difference between a BI tool connecting and a BI tool
// working.
//
// Values go out in text format. Binary would be marginally smaller and is
// what a driver prefers, but text is what every client can read, and the
// engine's own executors already disagree about the Go type a number arrives
// as: the DuckDB CLI hands back strings, pgx hands back typed values, and
// BigQuery hands back something else again. Normalising all of that into one
// text rendering per declared type is one conversion instead of three.

// PostgreSQL type OIDs, from pg_type. Only the ones a semantic result can
// contain: there is no array, no composite and no json here, because a
// dimension is a scalar by the time the model has accepted it.
const (
	oidBool        = 16
	oidInt8        = 20
	oidFloat8      = 701
	oidText        = 25
	oidNumeric     = 1700
	oidDate        = 1082
	oidTimestamp   = 1114
	oidTimestamptz = 1184
)

// typeLength is what RowDescription reports for a fixed-width type, and -1
// for a variable one. A client uses it to size its buffers; getting it wrong
// on a fixed type makes some drivers misparse.
func typeLength(oid uint32) int16 {
	switch oid {
	case oidBool:
		return 1
	case oidInt8, oidFloat8, oidTimestamp, oidTimestamptz:
		return 8
	case oidDate:
		return 4
	}
	return -1
}

// columnTypes decides one OID per column.
//
// A metric is numeric because the model says it is a measure, not because
// the value that arrived happened to look like a number. Executors disagree
// about the Go type: the DuckDB CLI returns every value as a string, pgx
// returns typed values, BigQuery returns something else again. Inferring
// from the value made the same metric numeric on one warehouse and text on
// another, and a client told text renders a measure as a label and will not
// plot it, so a dashboard would quietly come out with a category axis after
// a warehouse change.
//
// NUMERIC rather than a narrower numeric type because the text format is the
// same for all of them and NUMERIC is the one that never loses a digit. A
// count arrives as 5 and parses fine.
//
// A dimension is inferred from its first non-null value, and text when there
// is none. Getting a dimension's type slightly wrong costs a client nothing:
// it groups and filters either way.
func columnTypes(measure []bool, rows [][]any) []uint32 {
	oids := make([]uint32, len(measure))
	for i := range oids {
		if measure[i] {
			oids[i] = oidNumeric
			continue
		}
		oids[i] = oidText
		for _, row := range rows {
			if i >= len(row) || row[i] == nil {
				continue
			}
			oids[i] = oidOf(row[i])
			break
		}
	}
	return oids
}

func oidOf(v any) uint32 {
	switch v.(type) {
	case bool:
		return oidBool
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return oidInt8
	case float32, float64:
		return oidFloat8
	case *big.Rat, big.Rat:
		return oidNumeric
	case time.Time:
		return oidTimestamptz
	}
	return oidText
}

// encodeText renders one value for the wire, or nil for SQL NULL.
//
// A nil return is not an empty string: PostgreSQL distinguishes them with a
// length of -1, and a client that reads NULL as "" will chart a missing
// measure as a zero.
func encodeText(v any) []byte {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		return []byte(t)
	case []byte:
		return t
	case bool:
		// PostgreSQL's text format for booleans is t and f, not true/false.
		if t {
			return []byte("t")
		}
		return []byte("f")
	case int:
		return []byte(strconv.FormatInt(int64(t), 10))
	case int32:
		return []byte(strconv.FormatInt(int64(t), 10))
	case int64:
		return []byte(strconv.FormatInt(t, 10))
	case float32:
		return []byte(strconv.FormatFloat(float64(t), 'g', -1, 32))
	case float64:
		// 'g' with -1 gives the shortest representation that round-trips,
		// so a total does not gain or lose a digit on the way out.
		return []byte(strconv.FormatFloat(t, 'g', -1, 64))
	case *big.Rat:
		// A NUMERIC that came back exact stays exact. Rendering through a
		// float here would silently reintroduce the precision loss the
		// Postgres executor goes out of its way to avoid.
		return []byte(t.FloatString(ratScale(t)))
	case time.Time:
		return []byte(t.Format("2006-01-02 15:04:05.999999-07"))
	}
	return []byte(fmt.Sprint(v))
}

// ratScale keeps a rational's exact decimal places, up to a bound.
//
// An exact value prints exactly; a repeating one is cut off rather than
// printed forever. Division is the only thing that produces a repeating
// rational here, and docs/06-validation.md already records that division
// precision differs by warehouse.
func ratScale(r *big.Rat) int {
	if r.IsInt() {
		return 0
	}
	const maxScale = 16
	for scale := range maxScale {
		if exact, ok := new(big.Rat).SetString(r.FloatString(scale)); ok && exact.Cmp(r) == 0 {
			return scale
		}
	}
	return maxScale
}
