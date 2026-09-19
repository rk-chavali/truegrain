package plan

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rk-chavali/truegrain/internal/osi"
)

// Filter values arrive from outside the process: an agent's tool call, a REST
// body, a CLI flag. They are normalized and type-checked here, at the edge,
// before they reach the planner or any emitter.
//
// Normalization produces exactly one of: string, int64, float64, bool,
// time.Time. Every emitter binds these as query parameters, never as text, so
// this function is a correctness boundary rather than an injection defence. It
// is still the only place a value's shape is decided, which keeps the emitters
// free of coercion logic that could drift apart between dialects.

// dateLayouts are accepted for temporal dimensions, most specific first.
var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"15:04:05",
}

func normalizeValues(f *osi.Field, op Op, in []any) ([]any, error) {
	out := make([]any, 0, len(in))
	for i, raw := range in {
		v, err := normalizeOne(f, raw)
		if err != nil {
			return nil, &Refusal{Code: CodeBadFilter, Subject: f.QualifiedName(),
				Reason: fmt.Sprintf("value %d: %v", i+1, err),
				Hint:   hintForType(f)}
		}
		out = append(out, v)
	}
	if op == OpBetween {
		if err := checkRange(f, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeOne(f *osi.Field, raw any) (any, error) {
	if raw == nil {
		return nil, fmt.Errorf("null is not a filter value; use the is_null or is_not_null operator")
	}
	// json.Number preserves integer precision that float64 would lose.
	if n, ok := raw.(json.Number); ok {
		raw = string(n)
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			raw = i
		} else if fl, err := strconv.ParseFloat(string(n), 64); err == nil {
			raw = fl
		}
	}

	switch osi.Datatype(f.Datatype) {
	case osi.TypeInteger:
		return toInt(raw)
	case osi.TypeDecimal, osi.TypeFloat:
		return toFloat(raw)
	case osi.TypeBoolean:
		return toBool(raw)
	case osi.TypeDate:
		return toDate(raw)
	case osi.TypeTime, osi.TypeDateTime, osi.TypeDateTimeTz:
		return toTime(raw)
	case osi.TypeString:
		return toString(raw)
	}
	// Datatype absent or Opaque: accept any scalar unchanged, but still reject
	// structured values, which no dialect can bind.
	return toScalar(raw)
}

func toInt(raw any) (any, error) {
	switch v := raw.(type) {
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		if v != math.Trunc(v) {
			return nil, fmt.Errorf("%v is not a whole number", v)
		}
		return int64(v), nil
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", v)
		}
		return i, nil
	}
	return nil, fmt.Errorf("%T is not an integer", raw)
}

func toFloat(raw any) (any, error) {
	switch v := raw.(type) {
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%v is not a finite number", v)
		}
		return v, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", v)
		}
		return f, nil
	}
	return nil, fmt.Errorf("%T is not a number", raw)
}

func toBool(raw any) (any, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%q is not a boolean", v)
		}
		return b, nil
	}
	return nil, fmt.Errorf("%T is not a boolean", raw)
}

// Date is a calendar date: no clock, no zone.
//
// Distinct from time.Time because the distinction is real in the warehouse and
// gets lost otherwise. Found against BigQuery: a filter on a DATE column bound
// as a timestamp is rejected outright with "No matching signature for operator
// >=", because BigQuery will not compare DATE to TIMESTAMP and is right not to.
// Carrying the declared type this far lets each executor bind what its
// warehouse actually wants.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// String renders the ISO form every supported warehouse accepts.
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// after reports whether d falls later than other. The ISO form sorts
// lexicographically, which is the whole reason that format is worth using.
func (d Date) after(other Date) bool { return d.String() > other.String() }

// toDate parses a calendar date. It accepts the timestamp layouts too and
// keeps only the date part, because "on or after the first of March" is a
// reasonable thing to write as a timestamp and truncating is what the caller
// meant.
func toDate(raw any) (any, error) {
	v, err := toTime(raw)
	if err != nil {
		return nil, err
	}
	t := v.(time.Time)
	return Date{Year: t.Year(), Month: t.Month(), Day: t.Day()}, nil
}

func toTime(raw any) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%T is not a date or timestamp; pass it as a string", raw)
	}
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("%q is not a recognised date or timestamp", s)
}

func toString(raw any) (any, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	}
	return nil, fmt.Errorf("%T cannot be compared to a string dimension", raw)
}

func toScalar(raw any) (any, error) {
	switch v := raw.(type) {
	case string, bool, int64, float64, time.Time:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	}
	return nil, fmt.Errorf("%T is not a scalar value", raw)
}

// checkRange rejects a reversed BETWEEN. Left alone it matches no rows, and an
// empty result that looks like a real answer is the failure this engine exists
// to prevent.
func checkRange(f *osi.Field, vs []any) error {
	if len(vs) != 2 {
		return nil
	}
	reversed := false
	switch lo := vs[0].(type) {
	case int64:
		hi, ok := vs[1].(int64)
		reversed = ok && lo > hi
	case float64:
		hi, ok := vs[1].(float64)
		reversed = ok && lo > hi
	case string:
		hi, ok := vs[1].(string)
		reversed = ok && lo > hi
	case time.Time:
		hi, ok := vs[1].(time.Time)
		reversed = ok && lo.After(hi)
	case Date:
		// Dates need their own arm. They used to be time.Time and were caught
		// by the one above; giving them a distinct type quietly stopped a
		// reversed range being refused, which is the worst possible way to
		// fail here because it matches no rows and looks like a real answer.
		hi, ok := vs[1].(Date)
		reversed = ok && lo.after(hi)
	}
	if reversed {
		return &Refusal{Code: CodeBadFilter, Subject: f.QualifiedName(),
			Reason: fmt.Sprintf("between bounds are reversed: %v is greater than %v", vs[0], vs[1]),
			Hint:   "pass the lower bound first; as written this would match no rows and look like a real answer"}
	}
	return nil
}

func hintForType(f *osi.Field) string {
	if f.Datatype == "" {
		return fmt.Sprintf("dimension %s declares no datatype, so it accepts any scalar", f.QualifiedName())
	}
	base := fmt.Sprintf("dimension %s is declared %s", f.QualifiedName(), f.Datatype)
	switch osi.Datatype(f.Datatype) {
	case osi.TypeDate:
		return base + "; pass dates as \"2006-01-02\""
	case osi.TypeDateTime, osi.TypeDateTimeTz:
		return base + "; pass timestamps as RFC 3339, for example \"2026-01-31T00:00:00Z\""
	case osi.TypeTime:
		return base + "; pass times as \"15:04:05\""
	}
	return base
}
