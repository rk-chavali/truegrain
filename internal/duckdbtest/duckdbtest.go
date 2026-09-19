// Package duckdbtest seeds a throwaway DuckDB from the shared fixture.
//
// It exists because three test packages needed the same thing and two of
// them reached for build/demo.duckdb instead. That file is a gitignored
// build artifact: it is on the machine where somebody has run `make demo`
// and nowhere else, so the tests passed locally and failed in CI on a
// database that did not exist. A test that depends on an artifact is a test
// that depends on who ran what, which is the opposite of what a correctness
// suite is for.
//
// Every caller gets its own database in its own temporary directory, so
// tests cannot see each other's writes and nothing has to be cleaned up.
package duckdbtest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Seed builds a DuckDB holding testdata/fixtures/seed.sql and returns its
// path.
//
// The same fixture the parity suite runs against, so a number seen here is
// the number that suite proves. rootRelative is the path from the calling
// package back to the repository root, because a test's working directory
// is its own package directory.
func Seed(t *testing.T, rootRelative string) string {
	t.Helper()
	RequireBinary(t)

	db := filepath.Join(t.TempDir(), "fixture.duckdb")
	seed, err := os.ReadFile(filepath.Join(rootRelative, "testdata", "fixtures", "seed.sql"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	cmd := exec.Command("duckdb", db)
	cmd.Stdin = bytes.NewReader(seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding duckdb: %v\n%s", err, out)
	}
	return db
}

// RequireBinary skips when the DuckDB CLI is absent.
//
// A skip rather than a failure, so `go test ./...` works on a machine
// without it. CI installs it and fails the build on any skipped test, which
// is what stops the correctness suite quietly not running.
func RequireBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("the duckdb CLI is not on PATH; run `make duckdb`, or see .github/workflows/ci.yml")
	}
}
