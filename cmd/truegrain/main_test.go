package main_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The command layer is tested by running the built binary.
//
// It is worth the cost because this is where wiring bugs live rather than logic
// bugs, and wiring is invisible to a unit test. `semantic compile` once resolved
// access and discarded every denial, because it was constructed with a nil audit
// sink. Nothing below the command layer was wrong.

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "semantic-cli")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "semantic")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic("building the binary: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type run struct {
	stdout string
	stderr string
	code   int
}

func (r run) output() string { return r.stdout + r.stderr }

func semantic(t *testing.T, args ...string) run {
	t.Helper()
	cmd := exec.Command(binary, args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()

	result := run{stdout: out.String(), stderr: errOut.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case asExitError(err, &exitErr):
		result.code = exitErr.ExitCode()
	default:
		t.Fatalf("running %v: %v", args, err)
	}
	return result
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

func testdata(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...)
}

// ---------- validate ----------

func TestValidateAcceptsTheFixtureWorkspace(t *testing.T) {
	got := semantic(t, "validate", "-models", testdata("workspace"))
	if got.code != 0 {
		t.Fatalf("want exit 0, got %d:\n%s", got.code, got.output())
	}
	for _, want := range []string{"valid workspace", "sales", "marketing"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("output is missing %q:\n%s", want, got.stdout)
		}
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	dir := t.TempDir()
	// Two separate problems, so the run has something to report about each.
	writeFile(t, filepath.Join(dir, "broken.yaml"), `version: "0.2.0.dev0"
semantic_model:
  - name: broken
    description: Two problems at once.
    datasets:
      - name: things
        source: main.things
        description: No primary key, and the metric below has no description.
        fields:
          - name: amount
            expression:
              dialects: [{dialect: ANSI_SQL, expression: amount}]
            datatype: Decimal
            description: An amount.
    metrics:
      - name: total
        expression:
          dialects: [{dialect: ANSI_SQL, expression: "SUM(things.amount)"}]
`)
	got := semantic(t, "validate", "-models", dir)
	if got.code == 0 {
		t.Fatalf("a broken model must fail validation:\n%s", got.output())
	}
	// Reporting one problem per run turns fixing a model into a guessing game.
	if !strings.Contains(got.output(), "primary_key") || !strings.Contains(got.output(), "description") {
		t.Errorf("both problems should be reported together:\n%s", got.output())
	}
}

func TestValidateNamesTheFileAndLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.yaml"), "version: \"0.2.0.dev0\"\nsemantic_model:\n  - name: x\n    datasets: [}}}\n")
	got := semantic(t, "validate", "-models", dir)
	if got.code == 0 {
		t.Fatal("malformed YAML must fail validation")
	}
	if !strings.Contains(got.output(), "bad.yaml") {
		t.Errorf("the error should name the file:\n%s", got.output())
	}
}

// ---------- refusals and exit codes ----------

// TestRefusalUsesItsOwnExitCode lets a script tell "the engine declined" from
// "the invocation was wrong" from "it crashed".
func TestRefusalUsesItsOwnExitCode(t *testing.T) {
	got := semantic(t, "compile",
		"-models", testdata("workspace"),
		"-metric", "sales.order_revenue",
		"-dim", "sales.order_lines.item_id")

	if got.code != 3 {
		t.Fatalf("want exit 3 for a refusal, got %d:\n%s", got.code, got.output())
	}
	if !strings.Contains(got.stderr, "fan_out_would_inflate") {
		t.Errorf("the refusal should name its code:\n%s", got.stderr)
	}
	// The hint is the difference between a dead end and an answer.
	if !strings.Contains(got.stderr, "line_revenue") {
		t.Errorf("the hint should name a metric that can answer this:\n%s", got.stderr)
	}
}

func TestUnknownCommandExplainsItself(t *testing.T) {
	got := semantic(t, "frobnicate")
	if got.code == 0 {
		t.Fatal("an unknown command must not exit 0")
	}
	if !strings.Contains(got.stderr, "frobnicate") {
		t.Errorf("the error should name the command:\n%s", got.stderr)
	}
}

func TestCompileEmitsRunnableSQL(t *testing.T) {
	got := semantic(t, "compile",
		"-models", testdata("workspace"),
		"-metric", "sales.order_revenue",
		"-dim", "sales.customers.region")

	if got.code != 0 {
		t.Fatalf("want exit 0, got %d:\n%s", got.code, got.output())
	}
	// stdout carries only the SQL, so it can be piped straight into a client.
	if !strings.HasPrefix(strings.TrimSpace(got.stdout), "SELECT") {
		t.Errorf("stdout should be the statement alone:\n%s", got.stdout)
	}
	if !strings.HasSuffix(strings.TrimSpace(got.stdout), ";") {
		t.Errorf("the statement should be terminated:\n%s", got.stdout)
	}
	// Provenance goes to stderr so piping stdout stays clean.
	if !strings.Contains(got.stderr, "model version") {
		t.Errorf("provenance should go to stderr:\n%s", got.stderr)
	}
}

// ---------- the bug this file exists for ----------

// TestCompileAuditsDenials is the regression test for a real defect. `compile`
// resolves access, so it must record the decision. It was previously built with
// a nil audit sink, so every refusal on that path vanished. For a product whose
// claim is evidence, a command that decides silently is close to the worst
// possible failure.
func TestCompileAuditsDenials(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")

	got := semantic(t, "compile",
		"-models", testdata("workspace"),
		"-policy", testdata("workspace-policy.yaml"),
		"-identity", "growth@acme.com",
		"-audit", auditPath,
		"-metric", "marketing.attributed_revenue")

	if got.code != 3 {
		t.Fatalf("want a refusal, got exit %d:\n%s", got.code, got.output())
	}

	events := readAudit(t, auditPath)
	if len(events) != 1 {
		t.Fatalf("a denied compile must leave exactly one audit event, got %d", len(events))
	}
	event := events[0]
	if event["decision"] != "denied" {
		t.Errorf("want decision denied, got %v", event["decision"])
	}
	if event["identity"] != "growth@acme.com" {
		t.Errorf("the identity must be recorded, got %v", event["identity"])
	}
	if event["namespace"] != "marketing" {
		t.Errorf("the namespace is the first facet an evidence query filters on, got %v", event["namespace"])
	}
	if event["model_version"] == nil || event["model_version"] == "" {
		t.Error("the workspace digest must be recorded")
	}
	// The grant is written by sales against its own column, and marketing reads
	// it through an import. The audit records the origin, not the local alias.
	denied, _ := event["denied_fields"].([]any)
	if len(denied) == 0 || denied[0] != "sales.orders.order_total" {
		t.Errorf("the denied field should be recorded by its origin, got %v", denied)
	}
}

func TestQueryAuditsSuccesses(t *testing.T) {
	if _, err := exec.LookPath("duckdb"); err != nil {
		t.Skip("duckdb not on PATH")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "warehouse.duckdb")
	seed, err := os.ReadFile(testdata("fixtures", "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	load := exec.Command("duckdb", db)
	load.Stdin = strings.NewReader(string(seed))
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("seeding: %v\n%s", err, out)
	}

	auditPath := filepath.Join(dir, "audit.jsonl")
	got := semantic(t, "query",
		"-models", testdata("workspace"),
		"-db", db,
		"-audit", auditPath,
		"-identity", "analyst@example.com",
		"-metric", "sales.order_revenue")

	if got.code != 0 {
		t.Fatalf("want exit 0, got %d:\n%s", got.code, got.output())
	}
	events := readAudit(t, auditPath)
	if len(events) != 1 || events[0]["decision"] != "allowed" {
		t.Fatalf("an executed query must be recorded as allowed, got %v", events)
	}
	if events[0]["row_count"] == nil {
		t.Error("the row count should be recorded")
	}
	// Values and SQL text are deliberately absent: the log must not become a
	// second copy of the data it governs.
	if _, leaked := events[0]["compiled_sql"]; leaked {
		t.Error("the audit log must not carry the SQL text")
	}
}

// ---------- configuration ----------

// TestProjectFileIsDiscovered: a model repository is self-describing, so a
// command run inside it needs no flags at all.
func TestProjectFileIsDiscovered(t *testing.T) {
	dir := t.TempDir()
	// A workspace with its own project file, copied out of the fixture.
	copyTree(t, testdata("workspace"), dir)
	writeFile(t, filepath.Join(dir, "truegrain.yaml"), `version: 1
workspace:
  discover: ["domains/*"]
warehouse:
  dialect: bigquery
`)

	cmd := exec.Command(binary, "validate")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("validate with no flags should work inside a configured repo: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "truegrain.yaml") {
		t.Errorf("the output should name the config it found:\n%s", out)
	}
}

// TestExplicitFlagsBeatTheProjectFile lets a developer point a configured
// repository at a scratch target without editing anything.
func TestExplicitFlagsBeatTheProjectFile(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, testdata("workspace"), dir)
	writeFile(t, filepath.Join(dir, "truegrain.yaml"), "version: 1\nwarehouse:\n  dialect: bigquery\n")

	cmd := exec.Command(binary, "compile", "-dialect", "duckdb",
		"-metric", "sales.order_revenue")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, out)
	}
	// The DuckDB emitter quotes with double quotes; BigQuery uses backticks.
	if strings.Contains(string(out), "`main") {
		t.Errorf("the flag should have overridden the configured dialect:\n%s", out)
	}
}

func TestBrokenProjectFileFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, testdata("workspace"), dir)
	writeFile(t, filepath.Join(dir, "truegrain.yaml"),
		"version: 1\nwarehouse:\n  database: ${NOT_SET_ANYWHERE_AT_ALL}\n")

	cmd := exec.Command(binary, "validate")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an unset environment variable must fail the run:\n%s", out)
	}
	if !strings.Contains(string(out), "NOT_SET_ANYWHERE_AT_ALL") {
		t.Errorf("the error should name the variable:\n%s", out)
	}
}

// ---------- health ----------

// TestHealthStatesWhatIsNotEnforced. Overstating a governance guarantee is
// worse than not offering one, so the honest half must always be present.
func TestHealthStatesWhatIsNotEnforced(t *testing.T) {
	got := semantic(t, "health", "-models", testdata("workspace"))
	if got.code != 0 {
		t.Fatalf("want exit 0, got %d:\n%s", got.code, got.output())
	}
	if !strings.Contains(got.stdout, "does NOT enforce") {
		t.Errorf("health must state its limits:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "allow-all") {
		t.Errorf("an unconfigured deployment must name its resolver:\n%s", got.stdout)
	}
}

func TestHealthAsJSON(t *testing.T) {
	got := semantic(t, "health", "-models", testdata("workspace"), "-json")
	var payload map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &payload); err != nil {
		t.Fatalf("health -json must emit JSON: %v\n%s", err, got.stdout)
	}
	if payload["workspace_digest"] == nil {
		t.Error("health should carry the workspace digest")
	}
	if payload["enforcement_notes"] == nil {
		t.Error("health should carry the enforcement notes")
	}
}

// ---------- helpers ----------

func readAudit(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the audit log must be one JSON object per line: %v\n%s", err, line)
		}
		events = append(events, event)
	}
	return events
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copying the fixture: %v", err)
	}
}

// ---------- diff ----------

// diffWorkspaces copies the fixture workspace so a test can alter one side.
func diffWorkspaces(t *testing.T) (current, baseline string) {
	t.Helper()
	baseline = filepath.Join(t.TempDir(), "base")
	copyTree(t, testdata("workspace"), baseline)
	return testdata("workspace"), baseline
}

func TestDiffIsQuietWhenNothingChanged(t *testing.T) {
	current, baseline := diffWorkspaces(t)
	got := semantic(t, "diff", "-models", current, "-against", baseline)
	if got.code != 0 {
		t.Fatalf("identical workspaces must exit 0, got %d:\n%s", got.code, got.output())
	}
	if !strings.Contains(got.stdout, "No compiled result changed") {
		t.Errorf("unexpected output:\n%s", got.stdout)
	}
}

// TestDiffCatchesAChangedMeaning is the whole point of the command. The model
// still validates and every test still passes; only the compiled SQL moved, and
// with it every number anyone is looking at.
func TestDiffCatchesAChangedMeaning(t *testing.T) {
	current, baseline := diffWorkspaces(t)
	model := filepath.Join(baseline, "domains", "sales", "sales.yaml")
	body, err := os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(string(body), "status = 'shipped'", "status = 'delivered'", 1)
	if altered == string(body) {
		t.Fatal("the fixture no longer contains the expression this test alters")
	}
	writeFile(t, model, altered)

	got := semantic(t, "diff", "-models", current, "-against", baseline)

	if got.code != 4 {
		t.Fatalf("a changed compiled result must exit 4, got %d:\n%s", got.code, got.output())
	}
	if !strings.Contains(got.stdout, "shipped_revenue") {
		t.Errorf("the changed metric should be named:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "Changed meaning") {
		t.Errorf("the summary should say what kind of change it is:\n%s", got.stdout)
	}
}

func TestDiffShowsBothSidesWithFull(t *testing.T) {
	current, baseline := diffWorkspaces(t)
	model := filepath.Join(baseline, "domains", "sales", "sales.yaml")
	body, _ := os.ReadFile(model)
	writeFile(t, model, strings.Replace(string(body), "status = 'shipped'", "status = 'delivered'", 1))

	got := semantic(t, "diff", "-models", current, "-against", baseline, "-full")
	if !strings.Contains(got.stdout, "before:") || !strings.Contains(got.stdout, "after:") {
		t.Errorf("-full should print the SQL on both sides:\n%s", got.stdout)
	}
}

// TestDiffReportsRemovedMetrics: a metric that disappears breaks every caller
// asking for it, which is at least as serious as one that changed.
func TestDiffReportsRemovedMetrics(t *testing.T) {
	// The metric is dropped from the current side, so the baseline still has it
	// and the comparison sees a removal.
	trimmed := filepath.Join(t.TempDir(), "trimmed")
	copyTree(t, testdata("workspace"), trimmed)

	model := filepath.Join(trimmed, "domains", "marketing", "campaigns.yaml")
	body, err := os.ReadFile(model)
	if err != nil {
		t.Fatal(err)
	}
	const dropped = "      - name: return_on_spend"
	cut := strings.Index(string(body), dropped)
	if cut < 0 {
		t.Fatalf("the fixture no longer defines the metric this test removes (%s)", dropped)
	}
	writeFile(t, model, string(body)[:cut])

	got := semantic(t, "diff", "-models", trimmed, "-against", testdata("workspace"))

	if got.code != 4 {
		t.Fatalf("a removed metric must exit 4, got %d:\n%s", got.code, got.output())
	}
	if !strings.Contains(got.stdout, "Removed") || !strings.Contains(got.stdout, "return_on_spend") {
		t.Errorf("the removed metric should be named:\n%s", got.stdout)
	}
}

func TestDiffExitZeroReportsWithoutFailing(t *testing.T) {
	current, baseline := diffWorkspaces(t)
	model := filepath.Join(baseline, "domains", "sales", "sales.yaml")
	body, _ := os.ReadFile(model)
	writeFile(t, model, strings.Replace(string(body), "status = 'shipped'", "status = 'delivered'", 1))

	got := semantic(t, "diff", "-models", current, "-against", baseline, "-exit-zero")
	if got.code != 0 {
		t.Fatalf("-exit-zero must not fail, got %d", got.code)
	}
	if !strings.Contains(got.stdout, "Changed meaning") {
		t.Errorf("it should still report:\n%s", got.stdout)
	}
}

func TestDiffNeedsABaseline(t *testing.T) {
	got := semantic(t, "diff", "-models", testdata("workspace"))
	if got.code == 0 {
		t.Fatal("diff with no -against must fail")
	}
	if !strings.Contains(got.stderr, "worktree") {
		t.Errorf("the error should show how to produce a baseline in CI:\n%s", got.stderr)
	}
}

// TestOnlyTheFactoryBuildsAnExecutor guards the structural fix.
//
// Four commands used to construct DuckDB themselves. Adding BigQuery meant
// finding all four, and the one that got missed would have been a command that
// silently could not execute against the configured warehouse. Construction now
// lives in commonFlags.executor, and this keeps it there.
//
// It is the same lesson as the audit sink: governance and configuration that
// depend on every caller remembering will eventually be forgotten.
func TestOnlyTheFactoryBuildsAnExecutor(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name == "executor" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			if strings.HasPrefix(sel.Sel.Name, "New") {
				t.Errorf("%s builds exec.%s directly; every command must go through "+
					"commonFlags.executor, or adding a warehouse silently skips this one",
					fn.Name.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestStaticTokenNeedsAnIdentity.
//
// A static token is an identity claim: it says "I am X". Without X every
// audited decision records an empty subject, which cannot answer the one
// question an audit log exists to answer. Found by reading a real audit file
// after a live run, where `identity` was "" on every event.
func TestStaticTokenNeedsAnIdentity(t *testing.T) {
	models := filepath.Join("..", "..", "testdata", "workspace")

	cmd := exec.Command(binary, "serve", "rest",
		"-models", models, "-addr", "127.0.0.1:0", "-token-env", "TG_TEST_TOKEN")
	cmd.Env = append(os.Environ(), "TG_TEST_TOKEN=a-token")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatal("a static token with no identity must refuse to start")
	}
	if !strings.Contains(string(out), "-identity") {
		t.Errorf("the error must name the missing flag, got:\n%s", out)
	}
	if !strings.Contains(string(out), "empty subject") {
		t.Errorf("the error must say why it matters, got:\n%s", out)
	}
}

// TestADialectNeverRunsOnAnotherWarehousesExecutor.
//
// `executor` used to special-case BigQuery and fall through to DuckDB for
// everything else, so `-dialect postgres -db demo.duckdb` compiled Postgres
// SQL, executed it against a DuckDB file, and printed "dialect postgres" above
// the answer. It returned the correct number, because that statement was
// portable enough to survive the substitution.
//
// That is the worst shape a bug can take: it works until the emitted SQL stops
// being portable, and then the answer is wrong rather than absent. Postgres and
// Snowflake have no executor yet, so the only honest outcomes are compile-only
// or a refusal naming the mismatch.
func TestADialectNeverRunsOnAnotherWarehousesExecutor(t *testing.T) {
	// A path, not a database. The guard refuses on the flag being set, before
	// anything opens the file, so the file need not exist. That also keeps this
	// test from skipping, which the build treats as a failure.
	//
	// The assertion that carries the weight is the message, not the exit code.
	// Without the guard, DuckDB would be constructed, create this file, and fail
	// with something about a missing table: still a non-zero exit, but a message
	// that does not name the dialect.
	db := filepath.Join(t.TempDir(), "not-a-real.duckdb")

	for _, dialect := range []string{"postgres", "snowflake"} {
		t.Run(dialect, func(t *testing.T) {
			got := semantic(t, "query", "-models", testdata("workspace"),
				"-dialect", dialect, "-db", db, "-metric", "sales.order_revenue")

			if got.code == 0 {
				t.Fatalf("%s SQL was executed against a DuckDB file:\n%s", dialect, got.output())
			}
			// The message has to name the mismatch, or somebody reads "error"
			// and adds a credential rather than dropping the flag. Compared
			// case-insensitively: the Postgres branch writes "Postgres SQL"
			// because it reads better in a sentence, and asserting the flag's
			// own spelling would be a test about capitalisation.
			if !strings.Contains(strings.ToLower(got.output()), dialect) {
				t.Errorf("the refusal does not name the dialect:\n%s", got.output())
			}
		})
	}
}

// TestADialectWithNoExecutorStillCompiles, because compile-only is a complete
// configuration and refusing it would make `compile` useless for the two
// warehouses that cannot execute yet.
func TestADialectWithNoExecutorStillCompiles(t *testing.T) {
	for _, dialect := range []string{"postgres", "snowflake"} {
		got := semantic(t, "compile", "-models", testdata("workspace"),
			"-dialect", dialect, "-metric", "sales.order_revenue")

		if got.code != 0 {
			t.Errorf("%s must still compile without a warehouse:\n%s", dialect, got.output())
		}
		if !strings.Contains(got.stdout, "SELECT") {
			t.Errorf("no SQL for %s:\n%s", dialect, got.stdout)
		}
	}
}
