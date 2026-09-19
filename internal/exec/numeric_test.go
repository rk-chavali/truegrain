package exec

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"

	"github.com/rk-chavali/truegrain/internal/plan"
)

// BigQuery hands NUMERIC back as *big.Rat, and a rational number rendered
// carelessly is a fraction.
//
// This was live. Every monetary column came out of a real warehouse as `271/2`
// rather than `135.50`, through the CLI and through all three SDKs, because
// *big.Rat satisfied an `interface{ String() string }` meant for civil dates
// and got caught by it. Both String and MarshalText on big.Rat write a
// fraction, so the REST response carried it too. Nothing in the test suite
// noticed, because every test warehouse was DuckDB and DuckDB returns strings.

func TestNumericIsADecimalAndNeverAFraction(t *testing.T) {
	cases := map[string]struct {
		in   *big.Rat
		want string
	}{
		"the value that exposed this":   {big.NewRat(271, 2), "135.5"},
		"a whole number stays whole":    {big.NewRat(750, 1), "750"},
		"the fixture total":             {big.NewRat(1771, 2), "885.5"},
		"a third of a cent is not lost": {big.NewRat(1, 8), "0.125"},
		"negative money":                {big.NewRat(-2550, 100), "-25.5"},
		"zero":                          {big.NewRat(0, 1), "0"},
		"a large exact value":           {big.NewRat(123456789012, 100), "1234567890.12"},
	}

	for name, c := range cases {
		got := normalize(bigquery.Value(c.in))
		s, ok := got.(string)
		if !ok {
			t.Errorf("%s: want a string, got %T (%v)", name, got, got)
			continue
		}
		if s != c.want {
			t.Errorf("%s: got %q, want %q", name, s, c.want)
		}
	}
}

// TestNumericSurvivesJSON. The CLI is not the only consumer: the same value
// goes through the REST response into Python, TypeScript and Go, and big.Rat
// marshals itself as a fraction unless something has already converted it.
func TestNumericSurvivesJSON(t *testing.T) {
	raw, err := json.Marshal(normalize(bigquery.Value(big.NewRat(271, 2))))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if got := string(raw); got != `"135.5"` {
		t.Errorf("an SDK would receive %s", got)
	}

	// Proof the guard is needed rather than incidental: unconverted, this is
	// what every client was being sent.
	unconverted, _ := json.Marshal(big.NewRat(271, 2))
	if string(unconverted) != `"271/2"` {
		t.Logf("big.Rat now marshals as %s; the conversion may no longer be load bearing", unconverted)
	}
}

// TestDatesStillRender. The fix narrowed an interface match to three concrete
// types, so the types it was actually written for have to keep working.
func TestDatesStillRender(t *testing.T) {
	cases := map[string]struct {
		in   bigquery.Value
		want string
	}{
		"date":     {civil.Date{Year: 2026, Month: 3, Day: 5}, "2026-03-05"},
		"time":     {civil.Time{Hour: 14, Minute: 30}, "14:30:00"},
		"datetime": {civil.DateTime{Date: civil.Date{Year: 2026, Month: 3, Day: 5}}, "2026-03-05T00:00:00"},
	}
	for name, c := range cases {
		if got := normalize(c.in); got != c.want {
			t.Errorf("%s: got %v, want %q", name, got, c.want)
		}
	}
}

func TestTimestampIsRFC3339(t *testing.T) {
	ts := time.Date(2026, 3, 5, 14, 30, 0, 0, time.UTC)
	if got := normalize(bigquery.Value(ts)); got != "2026-03-05T14:30:00Z" {
		t.Errorf("got %v", got)
	}
}

func TestNilStaysNil(t *testing.T) {
	if got := normalize(bigquery.Value(nil)); got != nil {
		t.Errorf("want nil, got %v", got)
	}
	// A nil Rat reaching here would panic on Num(), and a result row is not
	// worth a crash.
	var r *big.Rat
	if got := normalize(bigquery.Value(r)); got != "" {
		t.Errorf("a nil rational should render empty, got %v", got)
	}
}

// A calendar date is not a timestamp, and BigQuery will not pretend otherwise.
//
// A filter on a DATE column bound as a time.Time becomes a TIMESTAMP
// parameter, and BigQuery rejects the comparison outright: "No matching
// signature for operator >= for argument types: DATE, TIMESTAMP". It is right
// to refuse. Every test warehouse until now was DuckDB, which accepts either,
// so this only appeared against a real DATE column in a real project.
func TestADateBindsAsADateNotATimestamp(t *testing.T) {
	bound := bindValue(plan.Date{Year: 2026, Month: time.February, Day: 1})

	d, ok := bound.(civil.Date)
	if !ok {
		t.Fatalf("a calendar date must bind as civil.Date, got %T", bound)
	}
	if d.String() != "2026-02-01" {
		t.Errorf("got %s", d)
	}

	// A real timestamp still binds as one: the point is that the model said
	// which, not that dates win.
	ts := time.Date(2026, 2, 1, 14, 30, 0, 0, time.UTC)
	if _, isDate := bindValue(ts).(civil.Date); isDate {
		t.Error("a timestamp was flattened into a date")
	}

	// Everything else passes through untouched.
	for _, v := range []any{"shipped", int64(5), 1.5, true} {
		if bindValue(v) != v {
			t.Errorf("%T was altered on the way to the warehouse", v)
		}
	}
}
