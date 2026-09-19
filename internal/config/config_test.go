package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rk-chavali/truegrain/internal/config"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `version: 1
warehouse:
  dialect: duckdb
`

func TestLoadsAProjectFile(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, config.FileName, `version: 1
workspace:
  discover: ["domains/*"]
warehouse:
  dialect: bigquery
  database: build/warehouse.duckdb
governance:
  policy: policy.yaml
audit:
  sink: file
  path: build/audit.jsonl
`)

	project, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if project.Warehouse.Dialect != "bigquery" {
		t.Errorf("dialect: got %q", project.Warehouse.Dialect)
	}
	if got := project.Workspace.Discover; len(got) != 1 || got[0] != "domains/*" {
		t.Errorf("discover: got %v", got)
	}
	// Paths resolve relative to the file, so the repository can be cloned
	// anywhere and run from any working directory.
	if !filepath.IsAbs(project.PolicyPath()) {
		t.Errorf("policy path should be absolute, got %q", project.PolicyPath())
	}
	if filepath.Dir(project.PolicyPath()) != project.Dir {
		t.Errorf("policy should resolve next to the file, got %q", project.PolicyPath())
	}
	if filepath.Base(project.AuditPath()) != "audit.jsonl" {
		t.Errorf("audit path: got %q", project.AuditPath())
	}
}

func TestUnsetPathsStayEmpty(t *testing.T) {
	// Empty must stay empty so a caller can tell "not configured" from
	// "configured to the project root".
	project, err := config.Load(write(t, t.TempDir(), config.FileName, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if project.PolicyPath() != "" {
		t.Errorf("an unset policy should resolve to empty, got %q", project.PolicyPath())
	}
	if project.DatabasePath() != "" {
		t.Errorf("an unset database should resolve to empty, got %q", project.DatabasePath())
	}
}

// ---------- environment interpolation ----------

func TestEnvironmentValuesAreSubstituted(t *testing.T) {
	t.Setenv("TEST_PROJECT_ID", "acme-analytics")
	path := write(t, t.TempDir(), config.FileName, `version: 1
warehouse:
  dialect: bigquery
  database: ${TEST_PROJECT_ID}/warehouse.duckdb
`)

	project, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(project.Warehouse.Database, "acme-analytics") {
		t.Errorf("the variable was not substituted: %q", project.Warehouse.Database)
	}
}

// TestUnsetVariableFailsTheLoad. An unset variable becoming an empty string is
// how a deployment silently connects to the wrong warehouse or loads no policy
// at all, and both are worse than not starting.
func TestUnsetVariableFailsTheLoad(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, `version: 1
warehouse:
  dialect: duckdb
  database: ${DEFINITELY_NOT_SET_ANYWHERE}
`)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("an unset environment variable must fail the load, not become empty")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ANYWHERE") {
		t.Errorf("the error should name the variable:\n%v", err)
	}
}

func TestEveryMissingVariableIsReportedAtOnce(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, `version: 1
warehouse:
  dialect: ${MISSING_ONE}
  database: ${MISSING_TWO}
`)

	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, name := range []string{"MISSING_ONE", "MISSING_TWO"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("all missing variables should be listed together, %q absent:\n%v", name, err)
		}
	}
}

// TestCommentsAreNotInterpolated is a regression test for a real bug. The
// substitution originally ran over the raw file text, so documenting the
// ${VAR} syntax inside the file made the file fail to load.
func TestCommentsAreNotInterpolated(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, `# Secrets are never in this file. Write them as ${SOME_SECRET} and they are
# read from the environment instead.
version: 1
warehouse:
  dialect: duckdb   # also fine to mention ${ANOTHER_ONE} here
`)

	project, err := config.Load(path)
	if err != nil {
		t.Fatalf("a comment mentioning the syntax must not be substituted:\n%v", err)
	}
	if project.Warehouse.Dialect != "duckdb" {
		t.Errorf("dialect: got %q", project.Warehouse.Dialect)
	}
}

// ---------- validation ----------

func TestUnsupportedVersionFails(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, "version: 99\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("an unsupported version must fail to load")
	}
}

func TestMissingVersionFails(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, "warehouse:\n  dialect: duckdb\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("a file with no version must fail to load")
	}
}

func TestUnknownAuditSinkFails(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, "version: 1\naudit:\n  sink: carrier-pigeon\n")
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("an unknown audit sink must fail to load")
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("the error should name the bad value:\n%v", err)
	}
}

// TestFileSinkNeedsAPath catches a configuration that looks like auditing and
// silently is not.
func TestFileSinkNeedsAPath(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, "version: 1\naudit:\n  sink: file\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("an audit sink of `file` with no path must fail to load")
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("loading a file that does not exist must fail")
	}
}

func TestMalformedYAMLIsAnError(t *testing.T) {
	path := write(t, t.TempDir(), config.FileName, "version: 1\nwarehouse: [unclosed\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("malformed YAML must fail to load")
	}
}

// ---------- discovery ----------

// TestDiscoverWalksUp is what lets a command run from inside a namespace
// directory, which is where someone editing a model actually is.
func TestDiscoverWalksUp(t *testing.T) {
	root := t.TempDir()
	write(t, root, config.FileName, minimal)
	deep := filepath.Join(root, "domains", "sales")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	found, ok := config.Discover(deep)
	if !ok {
		t.Fatal("discovery should walk up to the project root")
	}
	if filepath.Base(found) != config.FileName {
		t.Errorf("found %q", found)
	}
}

func TestDiscoverFindsTheNearestFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, config.FileName, minimal)
	nested := filepath.Join(root, "sub")
	write(t, nested, config.FileName, minimal)

	found, ok := config.Discover(nested)
	if !ok {
		t.Fatal("expected to find a project file")
	}
	if filepath.Dir(found) != nested {
		t.Errorf("the nearest file should win, got %q", found)
	}
}

// TestDiscoverStopsAtTheRoot: no project file is a perfectly good
// configuration, and must not be an error or an infinite walk.
func TestDiscoverStopsAtTheRoot(t *testing.T) {
	if _, ok := config.Discover(t.TempDir()); ok {
		t.Fatal("an empty tree should yield no project file")
	}
}

func TestResolveLeavesAbsolutePathsAlone(t *testing.T) {
	project, err := config.Load(write(t, t.TempDir(), config.FileName, minimal))
	if err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if got := project.Resolve(absolute); got != absolute {
		t.Errorf("an absolute path should pass through unchanged, got %q", got)
	}
}
