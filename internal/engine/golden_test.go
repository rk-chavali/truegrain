package engine_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

// update rewrites the golden files instead of comparing against them.
//
//	go test ./internal/engine/ -update
var update = flag.Bool("update", false, "rewrite golden multi-fact SQL")

// Golden SQL for multi-fact queries, committed so that a change to how facts
// are aggregated and joined appears as a readable diff in review. The combiner
// is the piece where a subtle mistake still returns plausible numbers, so the
// shape of its output is pinned rather than only its results.

var combinedCases = []struct {
	name string
	req  plan.Request
}{
	{"two_grains_no_dimensions", plan.Request{
		Metrics: []string{"sales.order_revenue", "sales.line_revenue"},
	}},
	{"two_grains_by_dimension", plan.Request{
		Metrics:    []string{"sales.order_revenue", "sales.line_revenue", "sales.units_sold"},
		Dimensions: []string{"sales.customers.region"},
		OrderBy:    []plan.Order{{Field: "region"}},
	}},
	{"cross_namespace_by_month", plan.Request{
		Metrics:    []string{"sales.order_revenue", "marketing.campaign_spend"},
		Dimensions: []string{"sales.orders.order_date"},
		Grain:      plan.GrainMonth,
		Limit:      24,
	}},
}

func TestGoldenCombinedSQL(t *testing.T) {
	for _, dialectName := range dialect.Names() {
		ws, err := workspace.Load(workspaceRoot(), workspace.Options{Strict: true})
		if err != nil {
			t.Fatal(err)
		}
		eng, err := engine.NewFromWorkspace(ws, engine.Config{
			Dialect: dialectName, Resolver: govern.AllowAll{},
		})
		if err != nil {
			t.Fatal(err)
		}

		for _, c := range combinedCases {
			t.Run(dialectName+"/"+c.name, func(t *testing.T) {
				compiled, err := eng.Compile(context.Background(),
					govern.Identity{Subject: "golden"}, c.req)
				if err != nil {
					t.Fatalf("compiling %s:\n%v", c.name, err)
				}
				got := compiled.SQL + "\n"
				if len(compiled.Params) > 0 {
					got += "\n-- parameters:\n"
					for i, p := range compiled.Params {
						got += fmt.Sprintf("--   $%d = %#v\n", i+1, p)
					}
				}

				path := filepath.Join("..", "..", "testdata", "golden",
					fmt.Sprintf("combined_%s.%s.sql", c.name, dialectName))
				if *update {
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("no golden file; run `go test ./internal/engine/ -update`:\n%v", err)
				}
				if normalize(string(want)) != normalize(got) {
					t.Errorf("combined SQL changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
				}
			})
		}
	}
}

func normalize(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
}
