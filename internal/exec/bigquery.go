package exec

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// BigQuery executes compiled SQL against BigQuery.
//
// Two things here are not conveniences.
//
// Queries run as the caller. The engine's promise is that access is the
// warehouse's own, not a privilege this process holds on everyone's behalf, so
// the executor impersonates the calling identity rather than sharing one
// service account. A deployment that has not configured impersonation still
// works, and says so in Name, because an unstated fallback to the process
// credentials is exactly the governance hole this project exists to close.
//
// Every query is estimated before it is run. A compiled statement against an
// unpartitioned table can scan terabytes, and the first time anyone notices is
// the invoice. The dry run refuses over a cap, and MaximumBytesBilled is set as
// a hard backstop in case the estimate is wrong.

// Refusal codes this executor adds.
const (
	CodeQueryTooLarge   = "query_scans_too_much"
	CodeWarehouseDenied = "warehouse_denied"
	// CodeNeedsPartitionFilter is a table that refuses to be scanned whole.
	// Answerable: the same question with a date filter works.
	CodeNeedsPartitionFilter = "needs_partition_filter"
)

func init() {
	// The query is answerable, just not at this breadth: filters or a coarser
	// grain will get it under the cap.
	plan.RegisterRetry(CodeQueryTooLarge, plan.RetryModify)
	// The warehouse's own IAM said no. No amount of rewriting helps.
	plan.RegisterRetry(CodeWarehouseDenied, plan.RetryNever)
	// Adding the filter the table asks for is exactly what fixes this.
	plan.RegisterRetry(CodeNeedsPartitionFilter, plan.RetryModify)
}

// DefaultMaxBytesBilled caps a single query at roughly a dollar of on-demand
// scan. It is deliberately low: a semantic layer answers aggregate questions
// over modelled tables, so a query that scans more than this is usually a
// missing partition filter rather than a legitimately large question.
const DefaultMaxBytesBilled int64 = 200 << 30 // 200 GiB

// BigQueryOptions configures the executor.
type BigQueryOptions struct {
	// Project is billed for the query and is where jobs are created.
	Project string
	// Location pins the job, for example "US" or "europe-west2". BigQuery
	// cannot always infer it, and an unpinned job against a regional dataset
	// fails with an error that does not mention the location.
	Location string
	// MaxBytesBilled caps a single query. Zero applies DefaultMaxBytesBilled;
	// a negative value disables the cap, which should be a deliberate choice
	// made by somebody who reads the bill.
	MaxBytesBilled int64
	// ImpersonateServiceAccounts runs each query as the calling identity.
	//
	// The process's own credentials need roles/iam.serviceAccountTokenCreator
	// on the identities it impersonates. Without this the executor runs every
	// query as itself, which makes the warehouse unable to distinguish callers
	// and defeats row and column security defined there.
	ImpersonateServiceAccounts bool
	// RequireImpersonation refuses a caller that cannot be impersonated rather
	// than running their query as this process.
	//
	// Off by default, because only a service account can be impersonated and
	// turning this on means an engine that serves no interactive users at all.
	// On is the correct setting wherever the warehouse's own row or column
	// security is load bearing, because with it off that security is evaluated
	// against this process for every human caller.
	RequireImpersonation bool
	// DryRunOnly estimates and refuses to run anything. Useful for validating
	// a model against a real warehouse's schema without spending.
	DryRunOnly bool
}

// BigQuery is an engine.Executor backed by the BigQuery API.
type BigQuery struct {
	opts    BigQueryOptions
	base    *bigquery.Client
	maxByte int64

	// clients caches one impersonated client per identity. Minting an
	// impersonated token is a network round trip, and a semantic layer answers
	// many small questions for the same few identities.
	mu      sync.Mutex
	clients map[string]*bigquery.Client
}

// NewBigQuery connects using Application Default Credentials.
//
// Credentials are never read from configuration: a key file path in a config
// file becomes a key file in a git repository.
func NewBigQuery(ctx context.Context, opts BigQueryOptions) (*BigQuery, error) {
	if opts.Project == "" {
		return nil, errors.New("a BigQuery project is required; set it with -project or BIGQUERY_PROJECT")
	}
	client, err := bigquery.NewClient(ctx, opts.Project)
	if err != nil {
		return nil, fmt.Errorf("connecting to BigQuery project %s: %w", opts.Project, err)
	}
	if opts.Location != "" {
		client.Location = opts.Location
	}

	maxBytes := opts.MaxBytesBilled
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytesBilled
	}
	return &BigQuery{opts: opts, base: client, maxByte: maxBytes, clients: map[string]*bigquery.Client{}}, nil
}

// Name describes the configuration, including what it does not enforce.
//
// health renders this verbatim. Overstating a governance guarantee is worse
// than not offering one, so a deployment running every query as its own
// service account says exactly that.
func (b *BigQuery) Name() string {
	parts := []string{"bigquery (" + b.opts.Project}
	if b.opts.Location != "" {
		parts = append(parts, ", "+b.opts.Location)
	}
	parts = append(parts, ")")
	name := strings.Join(parts, "")

	if b.opts.ImpersonateServiceAccounts {
		// The exact condition, not the headline. Only a service account can be
		// impersonated this way, so a human caller runs as this process, and an
		// operator who reads "queries run as the calling identity" and stops
		// there will believe BigQuery's own row and column security applies to
		// people it does not apply to.
		name += "; queries from service account callers run as that service account"
		if b.opts.RequireImpersonation {
			name += ", and a caller that cannot be impersonated is refused"
		} else {
			name += ", while any other caller runs as this process's own service" +
				" account, so warehouse-native access control does not distinguish them"
		}
	} else {
		name += "; impersonation is off, so every query runs as this process's own" +
			" service account and the warehouse cannot tell callers apart"
	}
	if b.maxByte > 0 {
		name += fmt.Sprintf("; capped at %s scanned per query", humanBytes(b.maxByte))
	}
	if b.opts.DryRunOnly {
		name += "; dry run only, no query is executed"
	}
	return name
}

// Execute estimates the query, refuses it if it is too broad, then runs it.
func (b *BigQuery) Execute(ctx context.Context, id govern.Identity, sql string, params []any) (*engine.Rows, error) {
	client, err := b.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}

	// The estimate happens first and against the same statement, so the number
	// reported in a refusal is the number the query would actually have cost.
	scanned, err := b.estimate(ctx, client, sql, params)
	if err != nil {
		return nil, err
	}
	if b.maxByte > 0 && scanned > b.maxByte {
		return nil, &plan.Refusal{
			Code: CodeQueryTooLarge,
			Reason: fmt.Sprintf("this query would scan %s, over the %s limit",
				humanBytes(scanned), humanBytes(b.maxByte)),
			Hint: "narrow it with a filter on the partitioning column, use a coarser " +
				"grain, or raise the limit deliberately with -max-bytes-billed",
		}
	}
	if b.opts.DryRunOnly {
		return nil, &plan.Refusal{
			Code:   CodeQueryTooLarge,
			Reason: fmt.Sprintf("this engine is in dry run mode; the query would scan %s", humanBytes(scanned)),
			Hint:   "restart without -dry-run to execute queries",
		}
	}

	q := b.query(client, sql, params)
	// A backstop in case the estimate was wrong, which it is for queries over
	// clustered tables and materialized views. BigQuery kills the job rather
	// than billing past this.
	if b.maxByte > 0 {
		q.MaxBytesBilled = b.maxByte
	}

	job, err := q.Run(ctx)
	if err != nil {
		return nil, b.refuse(err, "starting the query")
	}
	it, err := job.Read(ctx)
	if err != nil {
		return nil, b.refuse(err, "reading the result")
	}
	rows, err := readRows(it, job.ID())
	if err != nil {
		return nil, err
	}
	// What was actually billed, as opposed to what the dry run estimated. The
	// two disagree on clustered tables and materialized views, which is
	// precisely when someone wants to know. A failure to read the statistics
	// is not a failure of the query: the rows are already in hand, and losing
	// a cost number is not worth throwing them away.
	rows.BytesBilled = billedBytes(ctx, job)
	return rows, nil
}

// billedBytes reads the job statistics, or reports nothing.
func billedBytes(ctx context.Context, job *bigquery.Job) int64 {
	status, err := job.Status(ctx)
	if err != nil || status == nil || status.Statistics == nil {
		return 0
	}
	if q, ok := status.Statistics.Details.(*bigquery.QueryStatistics); ok && q != nil {
		return q.TotalBytesBilled
	}
	return 0
}

// Close releases the base client and every impersonated one.
func (b *BigQuery) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var firstErr error
	for _, c := range b.clients {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	b.clients = map[string]*bigquery.Client{}
	if err := b.base.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// clientFor returns a client that acts as id, or the process's own client when
// impersonation is off.
func (b *BigQuery) clientFor(ctx context.Context, id govern.Identity) (*bigquery.Client, error) {
	if !b.opts.ImpersonateServiceAccounts || id.Subject == "" {
		return b.base, nil
	}
	// Only a service account can be impersonated this way, so a human subject
	// cannot be, and the engine has to choose between refusing them and running
	// their query as itself.
	//
	// The default is to run it, because refusing means an engine that serves no
	// interactive users at all. That choice is reported by Name and repeated in
	// health, because the cost of it is real: BigQuery evaluates row and column
	// security against this process rather than against the caller, so the
	// warehouse cannot tell two human callers apart. A deployment relying on
	// that security sets RequireImpersonation and gives up interactive access.
	if !strings.HasSuffix(id.Subject, ".iam.gserviceaccount.com") {
		if b.opts.RequireImpersonation {
			return nil, &plan.Refusal{
				Code: CodeWarehouseDenied,
				Reason: fmt.Sprintf("%s is not a service account, so the query cannot run as them,"+
					" and this engine is configured never to run a query as itself", id.Subject),
				Hint: "call as a service account, or start the engine without" +
					" -impersonate-strict and accept that the warehouse sees one identity",
			}
		}
		return b.base, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.clients[id.Subject]; ok {
		return c, nil
	}

	source, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: id.Subject,
		Scopes:          []string{bigquery.Scope},
	})
	if err != nil {
		// Failing closed: running the query as this process instead would
		// silently grant the caller whatever the process can read.
		return nil, &plan.Refusal{
			Code: CodeWarehouseDenied,
			Reason: fmt.Sprintf("cannot act as %s, so the query was not run as the caller",
				id.Subject),
			Hint: "grant this deployment roles/iam.serviceAccountTokenCreator on that " +
				"service account, or start the engine without impersonation",
		}
	}
	client, err := bigquery.NewClient(ctx, b.opts.Project, option.WithTokenSource(source))
	if err != nil {
		return nil, fmt.Errorf("building an impersonated BigQuery client: %w", err)
	}
	if b.opts.Location != "" {
		client.Location = b.opts.Location
	}
	b.clients[id.Subject] = client
	return client, nil
}

// estimate dry runs the statement and returns the bytes it would scan.
func (b *BigQuery) estimate(ctx context.Context, client *bigquery.Client, sql string, params []any) (int64, error) {
	q := b.query(client, sql, params)
	q.DryRun = true
	job, err := q.Run(ctx)
	if err != nil {
		return 0, b.refuse(err, "estimating the query")
	}
	status := job.LastStatus()
	if status == nil || status.Statistics == nil {
		return 0, errors.New("BigQuery returned no statistics for the dry run")
	}
	return status.Statistics.TotalBytesProcessed, nil
}

// query builds a parameterized query. Values are bound as typed parameters and
// never concatenated into the statement.
func (b *BigQuery) query(client *bigquery.Client, sql string, params []any) *bigquery.Query {
	q := client.Query(sql)
	q.Parameters = namedParams(params)
	if b.opts.Location != "" {
		q.Location = b.opts.Location
	}
	return q
}

// namedParams binds positional values to the @p1, @p2 names the BigQuery
// dialect emits. The emitter and this function are the two halves of one
// convention; see internal/dialect/bigquery.go.
func namedParams(params []any) []bigquery.QueryParameter {
	out := make([]bigquery.QueryParameter, len(params))
	for i, v := range params {
		out[i] = bigquery.QueryParameter{Name: "p" + strconv.Itoa(i+1), Value: bindValue(v)}
	}
	return out
}

// bindValue converts a planner value into what BigQuery's driver expects.
//
// A calendar date has to arrive as civil.Date. Bound as a time.Time it becomes
// a TIMESTAMP, and BigQuery refuses to compare TIMESTAMP to a DATE column:
// "No matching signature for operator >=". It is right to refuse, and the
// failure only appears against a real warehouse with a real DATE column, which
// is where it was found.
func bindValue(v any) any {
	if d, ok := v.(plan.Date); ok {
		return civil.Date{Year: d.Year, Month: d.Month, Day: d.Day}
	}
	return v
}

// readRows materializes the result.
func readRows(it *bigquery.RowIterator, jobID string) (*engine.Rows, error) {
	columns := make([]string, 0, 8)
	if schema := it.Schema; schema != nil {
		for _, f := range schema {
			columns = append(columns, f.Name)
		}
	}

	rows := [][]any{}
	for {
		var values []bigquery.Value
		err := it.Next(&values)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading a result row: %w", err)
		}
		// The schema is only populated after the first page is fetched.
		if len(columns) == 0 && it.Schema != nil {
			for _, f := range it.Schema {
				columns = append(columns, f.Name)
			}
		}
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = normalize(v)
		}
		rows = append(rows, row)
	}
	return &engine.Rows{Columns: columns, Rows: rows, JobID: jobID}, nil
}

// normalize converts BigQuery's types into values that survive JSON encoding
// as something a caller can read.
func normalize(v bigquery.Value) any {
	switch t := v.(type) {
	case nil:
		return nil
	case time.Time:
		// RFC 3339 rather than a Unix epoch: a timestamp in a result is read
		// by a human or pasted into a report at least as often as it is parsed.
		return t.UTC().Format(time.RFC3339)
	case *big.Rat:
		// NUMERIC and BIGNUMERIC. This case must come before civilDateLike,
		// which *big.Rat also satisfies, and did: every monetary column came
		// back as `271/2` instead of `135.50`, through the CLI and through
		// every SDK, because big.Rat's String and MarshalText both write a
		// fraction.
		return decimalString(t)
	case civil.Date:
		return t.String()
	case civil.Time:
		return t.String()
	case civil.DateTime:
		return t.String()
	case []byte:
		return string(t)
	default:
		return v
	}
}

// decimalString renders an exact decimal, never a float.
//
// The scale is not recoverable: BigQuery reports no precision or scale for an
// aggregate, and sends 885.5 on the wire for a column declared NUMERIC(12,2).
// So this produces the same canonical form the warehouse itself does, and a
// value that reads as 135.50 from DuckDB reads as 135.5 here. The number is
// identical; only the trailing zeros differ, and inventing a scale would be
// guessing at how many belong.
//
// 38 digits because that is BIGNUMERIC's scale. FloatString rounds at the
// digit it is given, so a smaller number would silently discard precision from
// exactly the values chosen for not losing any.
func decimalString(r *big.Rat) string {
	if r == nil {
		return ""
	}
	if r.IsInt() {
		return r.Num().String()
	}
	s := strings.TrimRight(r.FloatString(38), "0")
	return strings.TrimSuffix(s, ".")
}

// refuse turns a BigQuery API error into a structured refusal where the cause
// is something the caller can act on.
//
// The message is passed through rather than summarized: BigQuery's errors name
// the column or table at fault, which is the actionable part.
func (b *BigQuery) refuse(err error, doing string) error {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 403:
			return &plan.Refusal{
				Code:   CodeWarehouseDenied,
				Reason: "the warehouse denied this query: " + apiErr.Message,
				Hint: "the caller's own BigQuery permissions decide this, not the " +
					"engine's policy file; grant access on the dataset or table",
			}
		case 400:
			// A 400 is not one thing. A partition filter requirement is
			// answerable by adding a filter, and telling an agent it is not
			// retryable makes it give up on a question it could have asked
			// correctly. Found running a model whose table required one.
			if needsPartitionFilter(apiErr.Message) {
				return &plan.Refusal{
					Code:   CodeNeedsPartitionFilter,
					Reason: "the warehouse requires a filter on the partitioning column: " + apiErr.Message,
					Hint: "add a filter on the date or timestamp column this table is " +
						"partitioned by. The requirement exists so a query cannot scan " +
						"the whole table by accident, and the column is named above.",
				}
			}
			return &plan.Refusal{
				Code:   plan.CodeUnsupported,
				Reason: "BigQuery rejected the compiled statement: " + apiErr.Message,
				Hint: "this usually means the model points at a column or table that " +
					"does not exist in this project; run validate against the warehouse",
			}
		}
	}
	return fmt.Errorf("%s: %w", doing, err)
}

// needsPartitionFilter recognises BigQuery refusing a query for scanning a
// partitioned table without a filter on its partitioning column.
//
// Matched on the message because BigQuery gives it the same 400 as a genuine
// syntax error, and the two need opposite advice: one says rewrite the
// request, the other says fix the model. The message names the column, which
// is the actionable part, so it is passed through rather than summarised.
func needsPartitionFilter(message string) bool {
	return strings.Contains(message, "without a filter over column") ||
		strings.Contains(message, "requires a filter over column")
}

// humanBytes renders a byte count the way a person reasons about a bill.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// Describe reports the columns BigQuery actually has for one table.
//
// Table metadata only. It reads no rows, so an engine that can run this needs
// no more than metadataViewer, and running it on a schedule costs nothing.
func (b *BigQuery) Describe(ctx context.Context, source string) (map[string]string, bool, error) {
	project, dataset, table := b.opts.Project, "", ""
	parts := strings.Split(strings.Trim(source, "`"), ".")
	switch len(parts) {
	case 2:
		dataset, table = parts[0], parts[1]
	case 3:
		project, dataset, table = parts[0], parts[1], parts[2]
	default:
		return nil, false, fmt.Errorf(
			"source %q is not a BigQuery table; expected dataset.table or project.dataset.table", source)
	}

	md, err := b.base.DatasetInProject(project, dataset).Table(table).Metadata(ctx)
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == 404 {
			// Absent, not unreadable. The difference decides whether somebody
			// goes looking at permissions or at the model.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading metadata for %s.%s.%s: %w", project, dataset, table, err)
	}

	out := map[string]string{}
	collectTypes(md.Schema, "", out)
	return out, true, nil
}

// collectTypes walks a schema, including nested RECORD fields, which are
// addressed by their dotted path exactly as BigQuery names them.
func collectTypes(schema bigquery.Schema, prefix string, out map[string]string) {
	for _, f := range schema {
		name := f.Name
		if prefix != "" {
			name = prefix + "." + f.Name
		}
		out[strings.ToLower(name)] = string(f.Type)
		if len(f.Schema) > 0 {
			collectTypes(f.Schema, name, out)
		}
	}
}

// Transient reports whether retrying the identical statement might succeed.
//
// A short allow list, for the reason the Postgres classifier gives: treating
// a real failure as passing costs money here. A 403 retried is three denied
// jobs, and a 400 on a bad column retried is the same rejection at triple
// the latency.
//
// The BigQuery client already retries at the transport layer, so most
// network flakiness never reaches this. What does reach it is a job-level
// failure: a rate limit when many callers submit at once, and a backend
// error during a regional incident. Both pass.
func (b *BigQuery) Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// A refusal is the engine's own verdict on the request, arrived at from
	// the warehouse's answer. It is never transient by construction: every
	// code that maps to one is a statement about the query.
	var refusal *plan.Refusal
	if errors.As(err, &refusal) {
		return false
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	return false
}
