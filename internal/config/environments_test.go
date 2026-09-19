package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/config"
)

// Environments.
//
// One set of model files, several deployments. The whole risk here is a
// number being right for one environment and read as though it came from
// another, so the tests are about the two ways that happens: an overlay
// that changes more or less than it looks like it does, and a deployment
// that cannot say which environment it is.

func projectFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "truegrain.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const twoEnvironments = `
version: 1
warehouse:
  dialect: duckdb
  database: build/demo.duckdb
  max_concurrent_queries: 4
governance:
  policy: policy.yaml
audit:
  sink: stdout

environments:
  dev:
    warehouse:
      database: build/dev.duckdb
  prod:
    warehouse:
      dialect: postgres
      dsn_env: TRUEGRAIN_PROD_DSN
      max_concurrent_queries: 8
    governance:
      policy: policy.prod.yaml
`

// TestAnOverlayChangesOnlyWhatItMentions.
//
// The merge rule, and the reason the overlays are held as raw nodes. An
// overlay that also reset the fields it did not mention would silently
// uncap concurrency and drop the policy file in whichever environment
// happened to be terser.
func TestAnOverlayChangesOnlyWhatItMentions(t *testing.T) {
	path := projectFile(t, twoEnvironments)

	dev, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.Use("dev"); err != nil {
		t.Fatal(err)
	}
	if dev.Warehouse.Database != "build/dev.duckdb" {
		t.Errorf("database = %q, want the overlay's", dev.Warehouse.Database)
	}
	if dev.Warehouse.Dialect != "duckdb" {
		t.Errorf("dialect = %q; dev does not mention it, so the base should stand",
			dev.Warehouse.Dialect)
	}
	if dev.Warehouse.MaxConcurrentQueries != 4 {
		t.Errorf("max_concurrent_queries = %d; dev does not mention it, so the base "+
			"cap should stand rather than being reset to zero, which means uncapped",
			dev.Warehouse.MaxConcurrentQueries)
	}
	if dev.Govern.Policy != "policy.yaml" {
		t.Errorf("policy = %q; dev does not mention it", dev.Govern.Policy)
	}
	if dev.Audit.Sink != "stdout" {
		t.Errorf("audit sink = %q; dev does not mention it", dev.Audit.Sink)
	}

	prod, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := prod.Use("prod"); err != nil {
		t.Fatal(err)
	}
	if prod.Warehouse.Dialect != "postgres" || prod.Warehouse.DSNEnv != "TRUEGRAIN_PROD_DSN" {
		t.Errorf("prod warehouse = %+v", prod.Warehouse)
	}
	if prod.Warehouse.MaxConcurrentQueries != 8 {
		t.Errorf("max_concurrent_queries = %d, want the overlay's 8",
			prod.Warehouse.MaxConcurrentQueries)
	}
	if prod.Govern.Policy != "policy.prod.yaml" {
		t.Errorf("policy = %q, want the overlay's", prod.Govern.Policy)
	}
	// prod does not mention database, so the base one is still there. That is
	// correct for the merge rule and is also why the dialect matters: nothing
	// runs a DuckDB file through the Postgres executor.
	if prod.Warehouse.Database != "build/demo.duckdb" {
		t.Errorf("database = %q; prod does not mention it", prod.Warehouse.Database)
	}
}

// TestATypedEnvironmentNameIsAnErrorNotAFallback.
//
// The guard that matters most. `-env prd` is a typo somebody will make, and
// falling back to the base configuration means serving development data
// under the belief that it is production, or the reverse. Both answer, and
// only one is right.
func TestATypedEnvironmentNameIsAnErrorNotAFallback(t *testing.T) {
	p, err := config.Load(projectFile(t, twoEnvironments))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Use("prd")
	if err == nil {
		t.Fatal("a misspelled environment name silently used the base configuration")
	}
	// The message has to list what does exist, or the person who typed it has
	// to go and read the file to find out what they meant.
	for _, want := range []string{"prd", "dev", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error omits %q: %v", want, err)
		}
	}
	if p.Environment != "" {
		t.Errorf("a failed Use left the project marked as environment %q", p.Environment)
	}
}

// TestChoosingNoEnvironmentIsAnErrorWhenAnyAreDeclared.
//
// A file with dev and prod in it plus an operator who forgot -env is a
// deployment nobody can name. Defaulting to the base configuration is the
// version of this that looks like it worked.
func TestChoosingNoEnvironmentIsAnErrorWhenAnyAreDeclared(t *testing.T) {
	p, err := config.Load(projectFile(t, twoEnvironments))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Use("")
	if err == nil {
		t.Fatal("a file declaring two environments loaded with neither chosen")
	}
	if !strings.Contains(err.Error(), "dev, prod") {
		t.Errorf("the error does not say what the choices are: %v", err)
	}
}

// TestAFileWithNoEnvironmentsIsUnaffected, because every project file that
// existed before this did not have the block and must keep loading.
func TestAFileWithNoEnvironmentsIsUnaffected(t *testing.T) {
	p, err := config.Load(projectFile(t, `
version: 1
warehouse:
  dialect: duckdb
  database: build/demo.duckdb
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Use(""); err != nil {
		t.Fatalf("a file with no environments block failed to load: %v", err)
	}
	if p.Environment != "" {
		t.Errorf("Environment = %q, want empty", p.Environment)
	}
	if err := p.Use("dev"); err == nil {
		t.Error("-env named an environment in a file that declares none, and it was accepted")
	}
}

// TestAnOverlayCannotRedefineTheEnvironments.
//
// An overlay that declared its own environments block would replace the map
// it was read from, so the set an operator read in the file and the set the
// process is using would differ. That reads as ordinary configuration and
// behaves as a trapdoor.
func TestAnOverlayCannotRedefineTheEnvironments(t *testing.T) {
	p, err := config.Load(projectFile(t, `
version: 1
warehouse:
  dialect: duckdb
environments:
  dev:
    warehouse:
      database: dev.duckdb
    environments:
      prod:
        warehouse:
          database: not-prod.duckdb
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Use("dev"); err == nil {
		t.Fatal("an overlay redefined the set of environments")
	}
}

// TestTheActiveEnvironmentIsRecorded, because every other guard here is
// about picking the right one and this is the only thing that lets anybody
// afterwards know which was picked.
func TestTheActiveEnvironmentIsRecorded(t *testing.T) {
	p, err := config.Load(projectFile(t, twoEnvironments))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Use("prod"); err != nil {
		t.Fatal(err)
	}
	if p.Environment != "prod" {
		t.Errorf("Environment = %q, want prod", p.Environment)
	}
}

// TestAnOverlayIsValidatedLikeTheBase, so a bad value reachable only in one
// environment fails at startup there rather than on the first audited
// decision.
func TestAnOverlayIsValidatedLikeTheBase(t *testing.T) {
	p, err := config.Load(projectFile(t, `
version: 1
warehouse:
  dialect: duckdb
audit:
  sink: stdout
environments:
  prod:
    audit:
      sink: file
`))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Use("prod")
	if err == nil {
		t.Fatal("an overlay set audit sink to file with no path and was accepted")
	}
	if !strings.Contains(err.Error(), "no path") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}
