// Command truegrain validates, compiles, queries and serves an Ossie semantic
// model.
//
// Every subcommand except `query` and `serve` works with no database and no
// credentials, which is what makes the quickstart in docs/08-contributing.md
// possible.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rk-chavali/truegrain/internal/config"
	"github.com/rk-chavali/truegrain/internal/dialect"
	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/gcp"
	"github.com/rk-chavali/truegrain/internal/gitsync"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/observe"
	"github.com/rk-chavali/truegrain/internal/plan"
	mcpserve "github.com/rk-chavali/truegrain/internal/serve/mcp"
	"github.com/rk-chavali/truegrain/internal/serve/rest"
	"github.com/rk-chavali/truegrain/internal/version"
	"github.com/rk-chavali/truegrain/internal/workspace"
)

const usage = `truegrain compiles a governed semantic model into warehouse SQL.

Usage:
  truegrain init      [flags]           read a warehouse and write a model to edit
  truegrain import    [flags]           carry an existing dbt semantic layer across
  truegrain validate  [flags]           check a model and report every problem
  truegrain test      [flags]           assert what the model answers and refuses
  truegrain compile   [flags]           print the SQL a request compiles to
  truegrain query     [flags]           compile, run, and print rows
  truegrain health    [flags]           report what this configuration enforces
  truegrain diff      [flags]           show which metrics compile differently
  truegrain doctor    [flags]           check the model against the live warehouse
  truegrain serve mcp  [flags]          serve the model to agents over MCP
  truegrain serve rest [flags]          serve the model over HTTP and JSON
  truegrain serve postgres [flags]      serve the model to BI tools over the
                                        PostgreSQL wire protocol
  truegrain version                    print the build, for a support request

Configuration comes from a truegrain.yaml in the model repository, found by
walking up from -models. Any flag given explicitly overrides it.

Common flags:
  -config   project file (default: nearest truegrain.yaml at or above -models)
  -models   workspace root, model directory, or a single Ossie file
  -dialect  target warehouse: %s (default duckdb)
  -policy   governance policy file; without one nothing is enforced
  -db       DuckDB database file; without one the engine compiles but does not run
  -identity workload identity the request runs as

Run "truegrain <command> -h" for the flags of one command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, strings.Join(dialect.Names(), ", "))
		os.Exit(2)
	}
	if err := run(os.Args[1:]); err != nil {
		// A refusal is an answer, not a crash. It prints without a stack and
		// with the exit code that distinguishes it from a broken invocation.
		fmt.Fprintln(os.Stderr, format(err))
		os.Exit(exitCode(err))
	}
}

func run(args []string) error {
	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "test":
		return cmdTest(args[1:])
	case "import":
		return cmdImport(args[1:])
	case "compile":
		return cmdCompile(args[1:])
	case "query":
		return cmdQuery(args[1:])
	case "health":
		return cmdHealth(args[1:])
	case "diff":
		return cmdDiff(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "serve":
		if len(args) < 2 {
			return fmt.Errorf("serve needs a transport: mcp, rest, postgres or console")
		}
		switch args[1] {
		case "mcp":
			return cmdServeMCP(args[2:])
		case "rest":
			return cmdServeREST(args[2:])
		case "console":
			return cmdServeConsole(args[2:])
		case "postgres":
			return cmdServePostgres(args[2:])
		}
		return fmt.Errorf("unknown transport %q, expected mcp, rest, postgres or console", args[1])
	case "version", "-version", "--version":
		return cmdVersion(args[1:])
	case "-h", "--help", "help":
		fmt.Printf(usage, strings.Join(dialect.Names(), ", "))
		return nil
	}
	return fmt.Errorf("unknown command %q; run `truegrain help`", args[0])
}

// exitCodeRefused marks a request the engine understood and declined, which a
// script distinguishes from a usage error or a crash.
const exitCodeRefused = 3

func exitCode(err error) int {
	var r *plan.Refusal
	if errors.As(err, &r) {
		return exitCodeRefused
	}
	return 1
}

// asRefusal reports whether err is a structured refusal, and binds it.
func asRefusal(err error, target **plan.Refusal) bool { return errors.As(err, target) }

func format(err error) string {
	var r *plan.Refusal
	if errors.As(err, &r) {
		var b strings.Builder
		fmt.Fprintf(&b, "refused (%s): %s", r.Code, r.Reason)
		if r.Hint != "" {
			fmt.Fprintf(&b, "\n  hint: %s", r.Hint)
		}
		return b.String()
	}
	return "error: " + err.Error()
}

// commonFlags are shared by every subcommand.
//
// Each one may come from the command line or from the project file a model
// repository carries. An explicit flag always wins, so a developer can point a
// configured repository at a scratch database without editing anything.
type commonFlags struct {
	fs      *flag.FlagSet
	project *config.Project
	config  *string
	// env names the overlay applied to the project file. See config.Use.
	env      *string
	models   *string
	dialect  *string
	policy   *string
	database *string
	audit    *string
	identity *string
	groups   *string

	// BigQuery. Empty project means compile only, which is the same thing an
	// empty -db means for DuckDB.
	bqProject     *string
	bqLocation    *string
	bqMaxBytes    *int64
	bqImpersonate *bool
	bqStrictImp   *bool
	dryRun        *bool
	policyTags    *bool

	// Postgres. An empty DSN variable means compile only, the same thing an
	// empty -project means for BigQuery and an empty -db means for DuckDB.
	pgDSNEnv *string

	// Git-backed models. Empty means -models is a path on disk, which is what
	// a developer on a laptop wants and what every test uses.
	gitTokenEnv *string
	gitDir      *string

	// Snowflake. An empty account means compile only, as above. The key is
	// named, never passed: a private key on a command line is in the
	// process list and in shell history.
	sfAccount   *string
	sfUser      *string
	sfKeyFile   *string
	sfKeyEnv    *string
	sfOAuthEnv  *string
	sfWarehouse *string
	sfDatabase  *string
	sfSchema    *string
	sfRole      *string

	// maxConcurrent bounds warehouse execution across every surface.
	maxConcurrent *int
	// cacheTTL serves a repeated question from memory. Zero does not cache.
	cacheTTL *time.Duration
}

func addCommon(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		fs:       fs,
		config:   fs.String("config", "", "project file; default is the nearest "+config.FileName+" at or above -models"),
		models:   fs.String("models", ".", "workspace root, model directory, or a single Ossie file"),
		dialect:  fs.String("dialect", "duckdb", "target warehouse: "+strings.Join(dialect.Names(), ", ")),
		policy:   fs.String("policy", "", "governance policy file; without one no access control is applied"),
		database: fs.String("db", "", "DuckDB database file; empty compiles without executing"),
		audit:    fs.String("audit", "", "append audit events as JSON lines to this file"),
		identity: fs.String("identity", "", "workload identity the request runs as"),
		groups:   fs.String("groups", "", "comma separated groups the identity belongs to"),
		env: fs.String("env", os.Getenv("TRUEGRAIN_ENV"), "environment to apply from the"+
			" project file's `environments:` block, for example dev or prod. Required"+
			" when the file declares any: defaulting would mean nobody can tell which"+
			" configuration answered. Defaults to $TRUEGRAIN_ENV"),

		bqProject:  fs.String("project", "", "BigQuery project billed for queries; empty compiles without executing"),
		bqLocation: fs.String("location", "", "BigQuery job location, for example US or europe-west2"),
		bqMaxBytes: fs.Int64("max-bytes-billed", 0,
			"refuse a query scanning more than this many bytes; 0 uses the default cap, -1 disables it"),
		policyTags: fs.Bool("policy-tags", false,
			"resolve column access from BigQuery policy tags instead of a policy file"),
		bqImpersonate: fs.Bool("impersonate", false,
			"run each query as the caller rather than as this process: a service account"+
				" on BigQuery, the caller's database role on Postgres"),
		bqStrictImp: fs.Bool("impersonate-strict", false,
			"with -impersonate, refuse a caller that cannot be impersonated instead of"+
				" running their query as this process; on BigQuery only service accounts"+
				" can be impersonated, so this turns off interactive access"),
		dryRun: fs.Bool("dry-run", false, "estimate every query against the warehouse but never run it"),

		cacheTTL: fs.Duration("cache", 0,
			"serve a repeated question from memory for this long instead of the"+
				" warehouse, for example 60s; zero does not cache. Health reports"+
				" the value, because a cached answer is a correct answer from a"+
				" moment ago rather than a correct answer"),

		maxConcurrent: fs.Int("max-concurrent-queries", 0,
			"refuse a query when this many are already running against the warehouse;"+
				" 0 does not cap. The right number is a property of the warehouse, so there"+
				" is no default: health reports when none is set"),

		pgDSNEnv: fs.String("dsn-env", "", "environment variable holding the PostgreSQL"+
			" connection string; empty compiles without executing. Named rather than"+
			" passed, because a connection string on the command line lands in shell"+
			" history and in the process list"),

		gitTokenEnv: fs.String("git-token-env", "", "environment variable holding a token"+
			" for a private model repository. Named rather than passed, for the same"+
			" reason -dsn-env is"),
		gitDir: fs.String("git-dir", "", "where to keep the working copy of a git-backed"+
			" -models; defaults to a directory under the user cache"),

		sfAccount: fs.String("snowflake-account", "", "Snowflake account identifier,"+
			" for example ab12345.us-east-1 or myorg-myaccount; empty compiles without executing"),
		sfUser: fs.String("snowflake-user", "", "Snowflake user the key pair belongs to"),
		sfKeyFile: fs.String("snowflake-key-file", "", "path to an unencrypted PKCS#8"+
			" private key for key-pair authentication"),
		sfKeyEnv: fs.String("snowflake-key-env", "", "environment variable holding the"+
			" PEM private key, as an alternative to -snowflake-key-file for a platform"+
			" secret manager that injects secrets as variables"),
		sfOAuthEnv: fs.String("snowflake-oauth-env", "", "environment variable holding an"+
			" OAuth access token, for a deployment that already has an OAuth integration."+
			" The SQL REST API does not accept a password, so there is no password option"),
		sfWarehouse: fs.String("snowflake-warehouse", "", "virtual warehouse that runs the query"),
		sfDatabase:  fs.String("snowflake-database", "", "default database for the session"),
		sfSchema:    fs.String("snowflake-schema", "", "default schema for the session"),
		sfRole:      fs.String("snowflake-role", "", "role the query runs under, unless -impersonate"),
	}
}

// resolve merges the project file into the flags. It runs after parsing and
// before anything reads a value.
func (c *commonFlags) resolve() error {
	given := map[string]bool{}
	c.fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	path := *c.config
	if path == "" {
		start := "."
		if given["models"] {
			start = *c.models
			if info, err := os.Stat(start); err == nil && !info.IsDir() {
				start = filepath.Dir(start)
			}
		}
		found, ok := config.Discover(start)
		if !ok {
			return nil // no project file is a perfectly good configuration
		}
		path = found
	}

	proj, err := config.Load(path)
	if err != nil {
		return err
	}
	// Before anything reads the project, because every field below can be
	// overlaid and reading one first would use the base value in a
	// deployment that had chosen an environment.
	if err := proj.Use(*c.env); err != nil {
		return err
	}
	c.project = proj

	if !given["models"] {
		*c.models = proj.ModelsPath()
	}
	if !given["dialect"] && proj.Warehouse.Dialect != "" {
		*c.dialect = proj.Warehouse.Dialect
	}
	if !given["policy"] && proj.PolicyPath() != "" {
		*c.policy = proj.PolicyPath()
	}
	if !given["db"] && proj.DatabasePath() != "" {
		*c.database = proj.DatabasePath()
	}
	if !given["dsn-env"] && proj.Warehouse.DSNEnv != "" {
		*c.pgDSNEnv = proj.Warehouse.DSNEnv
	}
	if !given["max-concurrent-queries"] && proj.Warehouse.MaxConcurrentQueries != 0 {
		*c.maxConcurrent = proj.Warehouse.MaxConcurrentQueries
	}
	if !given["project"] && proj.Warehouse.Project != "" {
		*c.bqProject = proj.Warehouse.Project
	}
	if !given["location"] && proj.Warehouse.Location != "" {
		*c.bqLocation = proj.Warehouse.Location
	}
	if !given["max-bytes-billed"] && proj.Warehouse.MaxBytesBilled != 0 {
		*c.bqMaxBytes = proj.Warehouse.MaxBytesBilled
	}
	if !given["impersonate"] && proj.Warehouse.Impersonate {
		*c.bqImpersonate = true
	}
	if !given["audit"] && proj.Audit.Sink == config.SinkFile {
		*c.audit = proj.AuditPath()
	}
	return nil
}

// discover returns the namespace discovery globs from the project file.
func (c *commonFlags) discover() []string {
	if c.project == nil {
		return nil
	}
	return c.project.Workspace.Discover
}

// origin describes where the configuration came from, for the banner.
func (c *commonFlags) origin() string {
	if c.project == nil {
		return "flags only (no " + config.FileName + " found)"
	}
	return c.project.File
}

func (c *commonFlags) id() govern.Identity {
	id := govern.Identity{Subject: *c.identity}
	if *c.groups != "" {
		for g := range strings.SplitSeq(*c.groups, ",") {
			if g = strings.TrimSpace(g); g != "" {
				id.Groups = append(id.Groups, g)
			}
		}
	}
	return id
}

// engine loads the workspace once and builds everything over it.
//
// The workspace is loaded before the policy so the policy can be validated
// against the names that actually exist. A policy naming a field the workspace
// does not define is refused rather than ignored, because a stale entry leaves
// a column everyone believed was protected readable.
//
// strict decides what a broken namespace does: commands where a human is
// watching fail, and a server isolates so one team cannot take the layer down.
// The audit sink is opened here rather than by each caller. Every command that
// reaches this function resolves access, so every one of them must record the
// decision. Leaving that to the caller is how `compile` ended up silently
// discarding denials, and the fix is to remove the choice.
// executor builds the executor for the configured warehouse, or nil for a
// compile-only engine.
//
// It lives here rather than in each command for the same reason the audit sink
// does. Four commands used to construct DuckDB themselves, which meant adding a
// warehouse required finding all four, and the one that got missed would be a
// command that silently could not execute. Removing the choice removes the bug.
//
// A nil executor is a complete configuration, not a failure: compile, validate
// and diff need no warehouse at all.
func (c *commonFlags) executor(ctx context.Context) (engine.Executor, func(), error) {
	noop := func() {}

	switch *c.dialect {
	case "bigquery":
		if *c.database != "" {
			return nil, noop, errors.New(
				"-db names a DuckDB file, which cannot run BigQuery SQL; " +
					"drop -db to query BigQuery, or pass -dialect duckdb")
		}
		if *c.bqProject == "" {
			return nil, noop, nil
		}
		bq, err := exec.NewBigQuery(ctx, exec.BigQueryOptions{
			Project:                    *c.bqProject,
			Location:                   *c.bqLocation,
			MaxBytesBilled:             *c.bqMaxBytes,
			ImpersonateServiceAccounts: *c.bqImpersonate,
			RequireImpersonation:       *c.bqStrictImp,
			DryRunOnly:                 *c.dryRun,
		})
		if err != nil {
			return nil, noop, err
		}
		return bq, func() { bq.Close() }, nil

	case "duckdb":
		if *c.database == "" {
			return nil, noop, nil
		}
		d, err := exec.NewDuckDB(exec.DuckDBOptions{Database: *c.database})
		if err != nil {
			return nil, noop, err
		}
		return d, func() { d.Close() }, nil

	case "postgres":
		if *c.database != "" {
			return nil, noop, errors.New(
				"-db names a DuckDB file, which cannot run Postgres SQL; " +
					"use -dsn-env to name the variable holding the connection string")
		}
		if *c.pgDSNEnv == "" {
			return nil, noop, nil
		}
		// Read the variable rather than the value. A connection string passed
		// as a flag is visible in shell history and to anything that can list
		// processes, and a password is the one value that must never be either.
		dsn := os.Getenv(*c.pgDSNEnv)
		if dsn == "" {
			return nil, noop, fmt.Errorf(
				"-dsn-env names %s, which is not set. Export the PostgreSQL connection "+
					"string in it, or drop -dsn-env to compile without executing", *c.pgDSNEnv)
		}
		pg, err := exec.NewPostgres(ctx, exec.PostgresOptions{
			DSN:               dsn,
			AssumeCallerRole:  *c.bqImpersonate,
			RequireCallerRole: *c.bqStrictImp,
		})
		if err != nil {
			return nil, noop, err
		}
		return pg, func() { pg.Close() }, nil

	case "snowflake":
		if *c.database != "" {
			return nil, noop, errors.New(
				"-db names a DuckDB file, which cannot run Snowflake SQL; " +
					"drop -db to compile without executing, or pass -dialect duckdb")
		}
		if *c.sfAccount == "" {
			return nil, noop, nil
		}
		key, oauth, err := c.snowflakeCredential()
		if err != nil {
			return nil, noop, err
		}
		sf, err := exec.NewSnowflake(exec.SnowflakeOptions{
			Account:           *c.sfAccount,
			User:              *c.sfUser,
			PrivateKeyPEM:     key,
			OAuthToken:        oauth,
			Warehouse:         *c.sfWarehouse,
			Database:          *c.sfDatabase,
			Schema:            *c.sfSchema,
			Role:              *c.sfRole,
			AssumeCallerRole:  *c.bqImpersonate,
			RequireCallerRole: *c.bqStrictImp,
		})
		if err != nil {
			return nil, noop, err
		}
		return sf, func() { sf.Close() }, nil
	}

	// Every other dialect compiles and has no executor. Refusing -db here is
	// the whole point of the switch: this function used to fall through to
	// DuckDB for anything that was not BigQuery, so `-dialect postgres -db
	// demo.duckdb` compiled Postgres SQL, ran it against a DuckDB file, and
	// printed "dialect postgres" over the answer. It returned the right number
	// on a statement portable enough to survive, which is the worst version of
	// that bug: it works until the SQL stops being portable, and then it is
	// wrong rather than broken.
	if *c.database != "" {
		return nil, noop, fmt.Errorf(
			"-db names a DuckDB file, which cannot run %s SQL; "+
				"drop -db to compile without executing, or pass -dialect duckdb to run against the file",
			*c.dialect)
	}
	return nil, noop, nil
}

// snowflakeCredential reads the private key or OAuth token from wherever the
// operator put it.
//
// A file path or an environment variable, never a flag value. A key on the
// command line is in the process list and in shell history, and a key is the
// one thing that cannot be rotated quietly once it has been somewhere else.
func (c *commonFlags) snowflakeCredential() (key []byte, oauth string, err error) {
	if *c.sfOAuthEnv != "" {
		token := os.Getenv(*c.sfOAuthEnv)
		if token == "" {
			return nil, "", fmt.Errorf(
				"-snowflake-oauth-env names %s, which is not set", *c.sfOAuthEnv)
		}
		return nil, token, nil
	}

	switch {
	case *c.sfKeyFile != "" && *c.sfKeyEnv != "":
		return nil, "", errors.New(
			"-snowflake-key-file and -snowflake-key-env both name a private key; " +
				"pick one, because which of the two is in use is not a thing to guess at")
	case *c.sfKeyFile != "":
		pemBytes, err := os.ReadFile(*c.sfKeyFile)
		if err != nil {
			return nil, "", fmt.Errorf("reading the Snowflake private key: %w", err)
		}
		return pemBytes, "", nil
	case *c.sfKeyEnv != "":
		value := os.Getenv(*c.sfKeyEnv)
		if value == "" {
			return nil, "", fmt.Errorf(
				"-snowflake-key-env names %s, which is not set", *c.sfKeyEnv)
		}
		return []byte(value), "", nil
	}
	return nil, "", errors.New(
		"-snowflake-account is set but no credential is: pass -snowflake-key-file, " +
			"-snowflake-key-env or -snowflake-oauth-env. The SQL REST API does not " +
			"accept a password")
}

// resolver chooses where column access comes from.
//
// Policy tags are checked first because they are the warehouse's own control:
// the tags are already on the columns and the caller's own IAM decides. A
// policy file is ours, which means it governs only queries that go through this
// engine and has to be kept in step by hand.
func (c *commonFlags) resolver(ws *workspace.Workspace) (govern.PolicyResolver, func(), error) {
	noop := func() {}

	if *c.policyTags {
		if *c.bqProject == "" {
			return nil, noop, errors.New(
				"-policy-tags needs -project, because policy tags are read from that project's table schemas")
		}
		if *c.policy != "" {
			// Silently preferring one would leave an operator believing the
			// other was in force.
			return nil, noop, errors.New(
				"-policy-tags and -policy are two different sources of truth for the same " +
					"decision; pass one")
		}
		ctx := context.Background()
		tags, err := gcp.NewSchemaTags(ctx, *c.bqProject)
		if err != nil {
			return nil, noop, err
		}
		return govern.NewTagResolver(tags, gcp.NewCatalogAccess()),
			func() { tags.Close() }, nil
	}

	if *c.policy != "" {
		pol, err := govern.LoadFilePolicy(*c.policy, ws)
		if err != nil {
			return nil, noop, err
		}
		return pol, noop, nil
	}
	return govern.AllowAll{}, noop, nil
}

// engine builds the engine. Extra sinks receive every decision alongside the
// configured one, which is how a server keeps a readable buffer without the
// file sink losing anything.
func (c *commonFlags) engine(ex engine.Executor, strict bool, extra ...govern.AuditSink) (*engine.Engine, func(), error) {
	audit, closeAudit, err := auditSink(*c.audit)
	if err != nil {
		return nil, nil, err
	}
	if len(extra) > 0 {
		audit = govern.Multi(append([]govern.AuditSink{audit}, extra...))
	}
	models, origin, err := c.modelPath()
	if err != nil {
		closeAudit()
		return nil, nil, err
	}
	ws, err := workspace.Load(models, workspace.Options{Discover: c.discover(), Strict: strict})
	if err != nil {
		closeAudit()
		return nil, nil, err
	}
	res, closeResolver, err := c.resolver(ws)
	if err != nil {
		closeAudit()
		return nil, nil, err
	}
	closeAll := func() { closeResolver(); closeAudit() }

	var cache *engine.Cache
	if *c.cacheTTL > 0 {
		var err error
		cache, err = engine.NewCache(engine.CacheOptions{TTL: *c.cacheTTL})
		if err != nil {
			closeAll()
			return nil, nil, err
		}
	}
	eng, err := engine.NewFromWorkspace(ws, engine.Config{
		Environment:          *c.env,
		Origin:               origin,
		Dialect:              *c.dialect,
		Resolver:             res,
		Audit:                audit,
		Executor:             ex,
		MaxConcurrentQueries: *c.maxConcurrent,
		Cache:                cache,
	})
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return eng, closeAll, nil
}

// cmdVersion prints what this build is.
//
// A binary that cannot say which commit produced it cannot be traced back to
// one. The audit log records the model version on every decision; this is the
// other half of the same question.
func cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	info := version.Get()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	fmt.Println(info)
	return nil
}

// ---------- validate ----------

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	common := addCommon(fs)
	verbose := fs.Bool("v", false, "list every metric and dimension")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("unknown -format %q: use text or json", *format)
	}
	if err := common.resolve(); err != nil {
		return err
	}

	models, _, err := common.modelPath()
	if err != nil {
		return err
	}

	// Strict: validate is the CI gate, so any namespace failing fails the run.
	ws, err := workspace.Load(models, workspace.Options{Discover: common.discover(), Strict: true})
	if err != nil {
		// A failing validate is the case a pipeline most needs to read, so JSON
		// has to survive it. Reporting the failure as a plain error would make
		// the one run worth parsing the one run that emits no JSON.
		if *format == "json" {
			writeValidationFailure(os.Stdout, *common.models, err)
			os.Exit(1)
		}
		return fmt.Errorf("workspace has errors:\n%w", err)
	}

	protected := 0
	if *common.policy != "" {
		pol, err := govern.LoadFilePolicy(*common.policy, ws)
		if err != nil {
			if *format == "json" {
				writeValidationFailure(os.Stdout, *common.policy, err)
				os.Exit(1)
			}
			return fmt.Errorf("policy has errors:\n%w", err)
		}
		protected = len(pol.ProtectedFields())
		if *format != "json" {
			fmt.Printf("%s is valid: %d protected field(s).\n\n", *common.policy, len(pol.ProtectedFields()))
		}
	}

	if *format == "json" {
		return writeValidation(os.Stdout, *common.models, common.origin(), *common.policy, protected, ws)
	}

	kind := "workspace"
	if ws.Legacy {
		kind = "model"
	}
	fmt.Printf("%s is a valid %s.\n", *common.models, kind)
	fmt.Printf("  config            %s\n", common.origin())
	fmt.Printf("  namespaces        %d\n", len(ws.Available()))
	fmt.Printf("  workspace digest  %s\n", ws.Digest)

	for _, ns := range ws.Available() {
		fmt.Printf("\n  %s\n", ns.Name)
		if owners := ns.Owners(); len(owners) > 0 {
			fmt.Printf("    owners         %s\n", strings.Join(owners, ", "))
		}
		fmt.Printf("    digest         %s\n", ns.Digest)
		fmt.Printf("    ossie spec     %s\n", ns.Model.SpecVersion)
		fmt.Printf("    datasets       %d\n", len(ns.Model.Datasets))
		fmt.Printf("    relationships  %d\n", len(ns.Model.Relationships))
		fmt.Printf("    metrics        %d\n", len(ns.Model.Metrics))
		fmt.Printf("    dimensions     %d\n", len(ns.Schema.Dimensions()))

		if !*verbose {
			continue
		}
		fmt.Println("\n    Metrics:")
		for _, m := range ns.Model.Metrics {
			fmt.Printf("      %-24s %s\n", workspace.Qualify(ns.Name, m.Name), m.Expression.ANSI())
		}
		fmt.Println("\n    Dimensions:")
		for _, f := range ns.Schema.Dimensions() {
			marker := " "
			if f.IsTime {
				marker = "t"
			}
			origin := ""
			if f.Dataset != nil && f.Dataset.ImportedFrom != "" {
				origin = "  (imported from " + f.Dataset.ImportedFrom + ")"
			}
			fmt.Printf("      %s %-38s %s%s\n",
				marker, workspace.Qualify(ns.Name, f.QualifiedName()), f.Description, origin)
		}
	}
	return nil
}

// ---------- request flags ----------

// requestFlags collect a semantic request from the command line. There is no
// flag that accepts SQL, on purpose: the CLI has the same vocabulary as every
// other interface.
type requestFlags struct {
	metrics    *string
	dimensions *string
	filters    *string
	grain      *string
	limit      *int
	orderBy    *string
}

func addRequest(fs *flag.FlagSet) *requestFlags {
	return &requestFlags{
		metrics:    fs.String("metric", "", "comma separated metric names (required)"),
		dimensions: fs.String("dim", "", "comma separated dimensions as dataset.field"),
		filters:    fs.String("filter", "", `JSON array of filters, for example '[{"dimension":"orders.status","op":"eq","values":["shipped"]}]'`),
		grain:      fs.String("grain", "", "time grain: "+grainList()),
		limit:      fs.Int("limit", 0, "maximum rows"),
		orderBy:    fs.String("order", "", "comma separated output columns, suffix with :desc to reverse"),
	}
}

func (r *requestFlags) request() (plan.Request, error) {
	req := plan.Request{
		Metrics:    splitList(*r.metrics),
		Dimensions: splitList(*r.dimensions),
		Grain:      plan.Grain(*r.grain),
		Limit:      *r.limit,
	}
	if *r.filters != "" {
		dec := json.NewDecoder(strings.NewReader(*r.filters))
		dec.UseNumber()
		if err := dec.Decode(&req.Filters); err != nil {
			return req, fmt.Errorf("parsing -filter: %w", err)
		}
	}
	for _, o := range splitList(*r.orderBy) {
		name, dir, _ := strings.Cut(o, ":")
		req.OrderBy = append(req.OrderBy, plan.Order{
			Field: name, Desc: strings.EqualFold(dir, "desc"),
		})
	}
	return req, nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func grainList() string {
	gs := plan.Grains()
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = string(g)
	}
	return strings.Join(out, ", ")
}

// ---------- compile ----------

func cmdCompile(args []string) error {
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	common := addCommon(fs)
	reqFlags := addRequest(fs)
	showParams := fs.Bool("params", true, "print the bind parameters below the SQL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	req, err := reqFlags.request()
	if err != nil {
		return err
	}

	eng, closeAudit, err := common.engine(nil, true)
	if err != nil {
		return err
	}
	defer closeAudit()

	c, err := eng.Compile(context.Background(), common.id(), req)
	if err != nil {
		return err
	}
	fmt.Println(c.SQL + ";")
	if *showParams && len(c.Params) > 0 {
		fmt.Println("\n-- parameters:")
		for i, p := range c.Params {
			fmt.Printf("--   %d = %v\n", i+1, p)
		}
	}
	fmt.Fprintf(os.Stderr, "\nmodel version %s, dialect %s\n", c.ModelVersion, c.Dialect)
	return nil
}

// ---------- query ----------

func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	common := addCommon(fs)
	reqFlags := addRequest(fs)
	format := fs.String("format", "table", "output format: table, csv or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	req, err := reqFlags.request()
	if err != nil {
		return err
	}

	ex, closeExec, err := common.executor(context.Background())
	if err != nil {
		return err
	}
	defer closeExec()

	eng, closeAudit, err := common.engine(ex, true)
	if err != nil {
		return err
	}
	defer closeAudit()
	res, err := eng.Query(context.Background(), common.id(), req)
	if err != nil {
		return err
	}
	return printResult(res, *format)
}

func auditSink(path string) (govern.AuditSink, func(), error) {
	if path == "" {
		return govern.DiscardAudit{}, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("opening audit file: %w", err)
	}
	return govern.NewJSONLAudit(f), func() { f.Close() }, nil
}

func printResult(res *engine.Result, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)

	case "csv":
		w := csv.NewWriter(os.Stdout)
		defer w.Flush()
		if err := w.Write(res.Columns); err != nil {
			return err
		}
		for _, row := range res.Rows {
			cells := make([]string, len(row))
			for i, v := range row {
				cells[i] = cell(v)
			}
			if err := w.Write(cells); err != nil {
				return err
			}
		}
		return w.Error()

	case "table":
		widths := make([]int, len(res.Columns))
		for i, c := range res.Columns {
			widths[i] = len(c)
		}
		rendered := make([][]string, len(res.Rows))
		for r, row := range res.Rows {
			rendered[r] = make([]string, len(res.Columns))
			for i := range res.Columns {
				if i < len(row) {
					rendered[r][i] = cell(row[i])
				}
				widths[i] = max(widths[i], len(rendered[r][i]))
			}
		}
		printRow(res.Columns, widths)
		seps := make([]string, len(widths))
		for i, w := range widths {
			seps[i] = strings.Repeat("-", w)
		}
		printRow(seps, widths)
		for _, row := range rendered {
			printRow(row, widths)
		}
		// Provenance goes to stderr so piping stdout gives clean data.
		fmt.Fprintf(os.Stderr, "\n%d row(s), model version %s, dialect %s\n",
			res.RowCount, res.ModelVersion, res.Dialect)
		fmt.Fprintf(os.Stderr, "\ncompiled SQL:\n%s\n", res.CompiledSQL)
		return nil
	}
	return fmt.Errorf("unknown format %q, expected table, csv or json", format)
}

func printRow(cells []string, widths []int) {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = fmt.Sprintf("%-*s", widths[i], c)
	}
	fmt.Println(strings.TrimRight(strings.Join(parts, "  "), " "))
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// ---------- health ----------

func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	common := addCommon(fs)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	// Build with the executor a query would use, so health describes the real
	// configuration rather than the one this command happens to need.
	ex, closeExec, err := common.executor(context.Background())
	if err != nil {
		return err
	}
	defer closeExec()
	eng, closeAudit, err := common.engine(ex, true)
	if err != nil {
		return err
	}
	defer closeAudit()
	h := eng.Health()
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(h)
	}
	fmt.Printf("workspace     %s (%s)\n", h.Workspace, h.WorkspaceDigest)
	fmt.Printf("ossie spec    %s\n", h.SpecVersion)
	fmt.Printf("dialect       %s\n", h.Dialect.Dialect)
	fmt.Printf("executor      %s\n", h.Executor)
	fmt.Printf("governance    %s\n", h.Governance.Resolver)
	fmt.Printf("              %s\n", h.Governance.Note)
	fmt.Printf("metrics       %d\n", h.Metrics)
	fmt.Printf("dimensions    %d\n", h.Dimensions)
	if len(h.Namespaces) > 1 || (len(h.Namespaces) == 1 && h.Namespaces[0].Owners != nil) {
		fmt.Println("\nNamespaces:")
		for _, ns := range h.Namespaces {
			state := "ok"
			if !ns.Available {
				state = "UNAVAILABLE"
			}
			fmt.Printf("  %-16s %-12s %2d metrics  %s\n",
				ns.Name, state, ns.Metrics, strings.Join(ns.Owners, ", "))
			if ns.Error != "" {
				fmt.Printf("      %s\n", strings.ReplaceAll(ns.Error, "\n", "\n      "))
			}
		}
	}
	if len(h.EnforcementNotes) > 0 {
		fmt.Println("\nWhat this configuration does NOT enforce:")
		for _, n := range h.EnforcementNotes {
			fmt.Printf("  - %s\n", n)
		}
	}
	return nil
}

// ---------- serve ----------

func cmdServeMCP(args []string) error {
	fs := flag.NewFlagSet("serve mcp", flag.ExitOnError)
	common := addCommon(fs)
	addr := fs.String("addr", "", "serve streamable HTTP on this address; empty uses stdio")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	ex, closeExec, err := common.executor(context.Background())
	if err != nil {
		return err
	}
	defer closeExec()
	// A server isolates a broken namespace rather than refusing to start.
	eng, closeAudit, err := common.engine(ex, false)
	if err != nil {
		return err
	}
	defer closeAudit()
	srv := mcpserve.New(eng, common.id()).MCPServer()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *addr == "" {
		// stdout is the protocol channel over stdio, so every log line goes to
		// stderr. A stray Println here corrupts the session.
		fmt.Fprintf(os.Stderr, "truegrain mcp: model %s (%s) on stdio\n", eng.ModelName(), eng.ModelVersion())
		return srv.Run(ctx, &mcp.StdioTransport{})
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return listenAndServe(ctx, *addr, handler, "mcp", eng, nil)
}

func cmdServeREST(args []string) error {
	fs := flag.NewFlagSet("serve rest", flag.ExitOnError)
	common := addCommon(fs)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	corsOrigins := fs.String("cors-origin", "",
		"comma separated origins a browser client may read responses from, for example http://localhost:5180")
	tokenEnv := fs.String("token-env", "", "environment variable holding a bearer token; without it the server is unauthenticated")
	tokenScopes := fs.String("token-scopes", "",
		"comma separated scopes the -token-env credential may use: "+
			strings.Join(rest.Scopes(), ", ")+"; empty grants all of them")
	obs := addObserve(fs)
	doctorEvery := fs.Duration("doctor-every", 0,
		"check the warehouse against the model this often, for example 30m;"+
			" zero never checks. Drift is found by looking regularly")
	testPath := fs.String("tests", "",
		"file or directory of model assertions, served at POST /v1/tests;"+
			" empty does not serve them at all")
	auditReaders := fs.String("audit-readers", "",
		"comma separated identities allowed to read recorded decisions over the API,"+
			" given as a subject or group:name; empty does not serve them at all")
	reload := fs.Duration("reload", 0,
		"re-read the model this often and serve it if it changed, for example 30s;"+
			" zero never reloads. A model that will not load is reported and the"+
			" previous one keeps answering")
	certFile := fs.String("tls-cert", "", "PEM certificate; serves HTTPS instead of HTTP")
	keyFile := fs.String("tls-key", "", "PEM private key, with -tls-cert")
	clientCAFile := fs.String("tls-client-ca", "",
		"PEM bundle of authorities whose client certificates are accepted."+
			" Turns on mutual TLS: a caller is identified by the certificate it"+
			" presents rather than by a bearer token, which is what a service"+
			" mesh already issues")
	certSubject := fs.String("tls-client-subject", "dns",
		"which part of a client certificate is the identity: dns, cn or uri."+
			" uri is where a SPIFFE ID lives")
	auth := authFlags{
		oidcIssuer:   fs.String("oidc-issuer", "", "OpenID Connect issuer URL; enables real token verification"),
		oidcAudience: fs.String("oidc-audience", "", "audience this deployment requires in a token; mandatory with -oidc-issuer"),
		subjectClaim: fs.String("subject-claim", "", "claim that becomes the identity (default email, falling back to sub)"),
		groupsClaim:  fs.String("groups-claim", "", "claim carrying group membership (default groups)"),
		googleAud:    fs.String("google-audience", "", "verify Google-signed identity tokens for this audience"),
		credentials: fs.String("credentials", "", "YAML file of bearer tokens, re-read"+
			" when it changes so a token can be rotated without a restart. Each entry"+
			" carries an id, a subject and an optional not_after, and two may be live"+
			" at once, which is what makes a rotation not an outage. The file holds"+
			" live credentials: mount it from the platform secret manager"),
	}
	jobStoreDSNEnv := fs.String("job-store-dsn-env", "",
		"environment variable holding a PostgreSQL connection string for a shared"+
			" job store; without it jobs live in this process and the deployment is"+
			" capped at one replica")
	webhookURL := fs.String("webhook", "",
		"POST every refusal and denial to this https endpoint as it happens,"+
			" so something can alert on a question the model cannot answer")
	webhookSecretEnv := fs.String("webhook-secret-env", "",
		"environment variable holding the HMAC secret the webhook body is"+
			" signed with; without one the receiver cannot tell this engine from"+
			" anybody who found the endpoint")
	revocations := fs.String("revocations", "",
		"YAML deny list of revoked subjects and token ids, re-read when it"+
			" changes so a credential can be turned off without a restart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	// Telemetry is configured before anything else can fail, so a bad endpoint
	// is reported as a startup error rather than as silence from a collector.
	lg, shutdownTelemetry, err := obs.start(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		// Its own deadline: the process is already stopping, and a collector
		// that has gone away must not hold the exit open.
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(flush); err != nil {
			lg.Warn("telemetry did not flush", slog.String("error", err.Error()))
		}
	}()

	ex, closeExec, err := common.executor(context.Background())
	if err != nil {
		return err
	}
	defer closeExec()
	// Kept whether or not anybody may read it: health reports how many
	// decisions this engine remembers, and that is useful on its own.
	recent := govern.NewRecentAudit(govern.DefaultRecentAudit)

	var hook *govern.Webhook
	if *webhookURL != "" {
		hook, err = govern.NewWebhook(govern.WebhookOptions{
			URL:    *webhookURL,
			Secret: os.Getenv(*webhookSecretEnv),
		})
		if err != nil {
			return err
		}
		defer func() { _ = hook.Close() }()
		if *webhookSecretEnv == "" {
			fmt.Fprintln(os.Stderr,
				"warning: the webhook is unsigned. Set -webhook-secret-env so the "+
					"receiver can tell this engine from anybody who found the endpoint")
		}
	}
	readers := rest.NewAuditReaders(splitList(*auditReaders))

	// A server isolates a broken namespace rather than refusing to start.
	// The webhook is an audit sink like any other, which is what keeps it
	// from becoming a second source of truth: it is told exactly what the
	// record is told, in the same already-scrubbed shape.
	sinks := []govern.AuditSink{recent}
	if hook != nil {
		sinks = append(sinks, hook)
	}

	eng, closeAudit, err := common.engine(ex, false, sinks...)
	if err != nil {
		return err
	}
	defer closeAudit()

	scopes, unknown := rest.ParseScopes(*tokenScopes)
	if len(unknown) > 0 {
		// Refused rather than dropped. A typo would otherwise produce a
		// credential narrower than intended, failing somewhere far from the
		// mistake, and the failure would look like a permissions problem.
		return fmt.Errorf("unknown scope(s) %s in -token-scopes; known scopes are %s",
			strings.Join(unknown, ", "), strings.Join(rest.Scopes(), ", "))
	}

	callerID := common.id()
	callerID.Scopes = scopes
	restTLS, err := restTLSConfig(*certFile, *keyFile, *clientCAFile)
	if err != nil {
		return err
	}

	var authenticator rest.Authenticator
	if *clientCAFile != "" {
		// Mutual TLS replaces the bearer token rather than adding to it.
		// Accepting either would mean the weaker one decides, which is not
		// what an operator who configured certificates asked for.
		authenticator = rest.ClientCertAuth{SubjectFrom: *certSubject}
		fmt.Fprintf(os.Stderr,
			"callers are identified by client certificate (%s)\n", *certSubject)
	} else {
		authenticator, err = auth.build(context.Background(), *tokenEnv, callerID, *addr)
		if err != nil {
			return err
		}
	}
	if *revocations != "" {
		// Wrapped around whichever authenticator was built, rather than built
		// into each: a deny list one of the four forgot to consult would be
		// documented as enforced and missing on the path somebody uses.
		list, err := govern.LoadRevocations(*revocations)
		if err != nil {
			return err
		}
		authenticator = rest.Revoking{Inner: authenticator, List: list}
		fmt.Fprintf(os.Stderr, "revocation list %s (%d entries)\n",
			*revocations, list.Count())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := rest.New(eng, authenticator).WithLogging(lg).
		WithAuditLog(recent, readers).
		WithModelTests(*testPath).
		WithDoctorSchedule(ctx, *doctorEvery, lg)

	if *jobStoreDSNEnv != "" {
		// The variable is named, not the value, the same rule every other
		// credential in this binary follows.
		dsn := os.Getenv(*jobStoreDSNEnv)
		if dsn == "" {
			return fmt.Errorf(
				"-job-store-dsn-env names %s, which is not set. Export a PostgreSQL "+
					"connection string for the engine's own state, which should be a "+
					"different database from the warehouse", *jobStoreDSNEnv)
		}
		store, err := rest.NewPostgresJobStore(ctx, dsn)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		srv = srv.WithJobStore(store)
		fmt.Fprintln(os.Stderr, "jobs are shared between replicas via PostgreSQL")
	}
	if *corsOrigins != "" {
		origins := strings.Split(*corsOrigins, ",")
		srv = srv.WithCORS(rest.NewCORS(origins))
		fmt.Fprintf(os.Stderr, "browser clients allowed from %s\n", *corsOrigins)
	}
	if *reload > 0 {
		// The same construction the server started with, so a reloaded model
		// gets the same audit sinks, resolver and executor as the first one.
		load := func() (*engine.Engine, func(), error) {
			return common.engine(ex, false, sinks...)
		}

		// Buffered one and sent to without blocking, so concurrent deploys
		// collapse into a single sync. Ten pipelines merging at once should
		// make the engine read the repository once, and the one read picks up
		// all ten merges anyway.
		trigger := make(chan struct{}, 1)
		go watchModel(ctx, srv, lg, *reload, load, eng.ModelVersion(), trigger)
		srv = srv.WithReload(func() error {
			select {
			case trigger <- struct{}{}:
			default:
			}
			return nil
		})
		lg.Info("watching the model for changes", slog.Duration("every", *reload))
	}
	if *obs.otlpEndpoint != "" {
		lg.Info("telemetry enabled",
			slog.String("endpoint", *obs.otlpEndpoint),
			slog.Float64("trace_sample", *obs.otlpSample))
	}
	return listenAndServe(ctx, *addr, srv.Handler(), "rest", eng, restTLS)
}

// observeFlags collects logging and telemetry, which are separate decisions.
//
// Logging is on by default, because a server that says nothing about what it
// did is one nobody can operate. Telemetry is off by default, because it opens
// a connection to somewhere and that has to be asked for.
type observeFlags struct {
	otlpEndpoint *string
	otlpInsecure *bool
	otlpSample   *float64
	otlpService  *string
	logLevel     *string
	logFormat    *string
}

func addObserve(fs *flag.FlagSet) observeFlags {
	return observeFlags{
		otlpEndpoint: fs.String("otlp-endpoint", "",
			"OTLP gRPC collector as host:port, for traces and metrics; empty disables telemetry"),
		otlpInsecure: fs.Bool("otlp-insecure", false,
			"send to the collector without TLS, for one on the same host or in the same pod"),
		otlpSample: fs.Float64("otlp-sample", 1.0,
			"fraction of traces recorded, 0 to 1; metrics are never sampled"),
		otlpService: fs.String("otlp-service", "truegrain",
			"name this deployment reports to the collector"),
		logLevel:  fs.String("log-level", "info", "debug, info, warn or error"),
		logFormat: fs.String("log-format", "json", "json or text"),
	}
}

// start builds the logger and installs telemetry. The returned shutdown must
// run before exit or the last few seconds of telemetry are lost, which is
// exactly the telemetry someone wants after a crash.
func (o observeFlags) start(ctx context.Context) (*slog.Logger, observe.Shutdown, error) {
	level, ok := observe.ParseLevel(*o.logLevel)
	if !ok {
		return nil, nil, fmt.Errorf("unknown -log-level %q: use debug, info, warn or error", *o.logLevel)
	}
	format := observe.LogFormat(strings.ToLower(strings.TrimSpace(*o.logFormat)))
	if format != observe.LogJSON && format != observe.LogText {
		return nil, nil, fmt.Errorf("unknown -log-format %q: use json or text", *o.logFormat)
	}
	// Logs go to stderr. Anything a server writes to stdout is somebody's data
	// on the CLI paths, and MCP speaks its protocol there.
	lg := observe.NewLogger(os.Stderr, format, level)

	shutdown, err := observe.Start(ctx, observe.Config{
		Endpoint:    *o.otlpEndpoint,
		Insecure:    *o.otlpInsecure,
		Service:     *o.otlpService,
		Version:     version.Get().Version,
		SampleRatio: *o.otlpSample,
	})
	if err != nil {
		return nil, nil, err
	}
	return lg, shutdown, nil
}

// restAuth builds the authenticator. A token is read from the environment,
// never from a flag, because a flag value is visible in the process list.
// authFlags collects the ways a deployment can prove who is calling.
type authFlags struct {
	oidcIssuer   *string
	oidcAudience *string
	subjectClaim *string
	groupsClaim  *string
	googleAud    *string
	credentials  *string
}

// build chooses an authenticator, most specific first.
//
// Everything this engine promises about governance rests on the identity being
// the caller's rather than the server's, so the weaker options narrow as they
// get weaker: a static token is one identity for everyone who holds it, and
// anonymous is refused anywhere but loopback.
func (a authFlags) build(ctx context.Context, tokenEnv string, id govern.Identity, addr string) (rest.Authenticator, error) {
	switch {
	case *a.credentials != "":
		// Ahead of -token-env deliberately. An operator who configured both
		// has a file that rotates and a variable that cannot, and picking
		// the variable would make the rotation silently do nothing.
		if tokenEnv != "" {
			return nil, errors.New(
				"-credentials and -token-env both configure bearer tokens. Use " +
					"-credentials: it is the one that can be rotated without a restart")
		}
		creds, err := govern.LoadCredentials(*a.credentials)
		if err != nil {
			return nil, err
		}
		return rest.RotatingTokens{Credentials: creds}, nil

	case *a.oidcIssuer != "":
		return rest.NewOIDC(ctx, rest.OIDCOptions{
			Issuer:       *a.oidcIssuer,
			Audience:     *a.oidcAudience,
			SubjectClaim: *a.subjectClaim,
			GroupsClaim:  *a.groupsClaim,
		})

	case *a.googleAud != "":
		return rest.NewGoogleIDToken(*a.googleAud, *a.groupsClaim)

	case tokenEnv != "":
		token := os.Getenv(tokenEnv)
		if token == "" {
			return nil, fmt.Errorf("environment variable %s is empty; it must hold the bearer token", tokenEnv)
		}
		// A static token is an identity claim: it says "I am X". Without X the
		// audit log records an empty subject on every decision, which cannot
		// answer the one question an audit log exists to answer.
		if id.Subject == "" {
			return nil, errors.New(
				"-token-env needs -identity naming who that token authenticates as; " +
					"without it every audited decision records an empty subject")
		}
		// One token means one identity, so every caller holding it is the
		// same subject in the audit log and to the policy file. Fine for a
		// single tenant, wrong the moment two callers must be told apart.
		if !isLoopback(addr) {
			fmt.Fprintf(os.Stderr,
				"warning: a static token maps every caller to %q, so the audit log cannot\n"+
					"         tell them apart. Use -oidc-issuer or -google-audience for real identity.\n",
				id.Subject)
		}
		return rest.StaticTokens{Tokens: map[string]govern.Identity{token: id}}, nil
	}

	if !isLoopback(addr) {
		return nil, fmt.Errorf(
			"refusing to serve on %s without authentication; use -oidc-issuer, "+
				"-google-audience, -credentials naming a token file, or -token-env naming "+
				"an environment variable that holds a bearer token", addr)
	}
	// Stamped explicitly rather than left empty, so the audit log says
	// something true about who asked rather than nothing at all.
	if id.Subject == "" {
		id.Subject = "anonymous"
	}
	fmt.Fprintf(os.Stderr,
		"warning: no authentication configured, so every request runs as %q.\n"+
			"         this is only safe because %s is loopback.\n",
		id.Subject, addr)
	return rest.AnonymousAuth{Identity: id}, nil
}

func isLoopback(addr string) bool {
	host, _, found := strings.Cut(addr, ":")
	if !found {
		return false
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
}

func listenAndServe(ctx context.Context, addr string, h http.Handler, kind string,
	eng *engine.Engine, tlsConfig *tls.Config) error {

	srv := &http.Server{
		Addr:    addr,
		Handler: h,
		// A slow client must not be able to hold a connection open forever.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         tlsConfig,
	}
	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}
	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "truegrain %s: model %s (%s) on %s://%s\n",
			kind, eng.ModelName(), eng.ModelVersion(), scheme, addr)
		if tlsConfig != nil {
			// The certificate already lives in TLSConfig, so the file
			// arguments are empty: passing them again would load a second
			// copy and let the two disagree.
			errCh <- srv.ListenAndServeTLS("", "")
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// restTLSConfig builds the listener's TLS, including mutual TLS when a
// client CA is given.
func restTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	switch {
	case certFile == "" && keyFile == "" && clientCAFile == "":
		return nil, nil
	case clientCAFile != "" && (certFile == "" || keyFile == ""):
		return nil, errors.New(
			"-tls-client-ca needs -tls-cert and -tls-key: mutual TLS is still " +
				"TLS, and the server has to present a certificate of its own")
	case certFile == "" || keyFile == "":
		return nil, errors.New("-tls-cert and -tls-key go together")
	}

	if clientCAFile != "" {
		return rest.MutualTLSConfig(certFile, keyFile, clientCAFile)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the TLS certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// modelPath resolves -models to a directory on disk.
//
// A plain path is returned unchanged, which is every developer on a laptop and
// every test. A `git+` value is cloned or fetched first, and the directory
// inside the working copy is returned instead.
//
// Called on every reload rather than once at startup. That is the whole
// mechanism: watchModel already rebuilds the engine on a timer and swaps only
// when the model digest changes, so syncing here means a merge to the tracked
// branch becomes the served model on the next tick, and a repository that will
// not clone leaves the previous model answering.
func (c *commonFlags) modelPath() (string, engine.Origin, error) {
	if !strings.HasPrefix(*c.models, gitsync.Prefix) {
		return *c.models, engine.Origin{}, nil
	}

	src, err := gitsync.Parse(*c.models)
	if err != nil {
		return "", engine.Origin{}, err
	}
	src.TokenEnv = *c.gitTokenEnv

	src.Dir, err = c.workingCopy(src)
	if err != nil {
		return "", engine.Origin{}, err
	}

	// Bounded, because a clone that hangs on an unreachable host would
	// otherwise hold the reload loop open until the process is killed, and a
	// reload loop that never ticks again is an engine that silently stops
	// tracking git.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	path, commit, err := src.Sync(ctx)
	if err != nil {
		return "", engine.Origin{}, err
	}
	return path, engine.Origin{
		Repository: src.URL,
		Ref:        src.Ref,
		Commit:     commit,
		Subdir:     src.Subdir,
	}, nil
}

// workingCopy decides where the clone lives.
//
// One directory per repository and ref, keyed by a hash of both, so pointing
// two engines at two branches of the same repository does not have them
// fighting over one checkout.
func (c *commonFlags) workingCopy(src gitsync.Source) (string, error) {
	if *c.gitDir != "" {
		return *c.gitDir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		// A container with no HOME set reaches here. Saying which flag fixes
		// it beats reporting whatever os.UserCacheDir decided to call the
		// problem.
		return "", fmt.Errorf(
			"there is no user cache directory to keep the working copy in (%w); "+
				"pass -git-dir naming a writable directory", err)
	}
	sum := sha256.Sum256([]byte(src.URL + "\n" + src.Ref))
	return filepath.Join(base, "truegrain", "models-"+hex.EncodeToString(sum[:])[:16]), nil
}
