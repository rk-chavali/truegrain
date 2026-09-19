package exec

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rk-chavali/truegrain/internal/engine"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// Snowflake executes compiled SQL against Snowflake's SQL REST API.
//
// The fourth warehouse, and the last one whose dialect had never executed a
// statement. Everything below has been built against Snowflake's documented
// protocol and has not been run against an account, which is stated in
// Name() rather than left for somebody to discover.
//
// Three decisions are worth the reader's time.
//
// The REST API rather than the Go driver. gosnowflake is the official
// client and it is a large dependency: it pulls the AWS, Azure and Google
// storage SDKs in so that it can stage files, which is a capability this
// engine will never use because it only ever issues aggregated SELECTs.
// /api/v2/statements needs net/http and about two hundred lines, keeps the
// build pure Go, and cannot be talked into writing anything. The cost is
// real and is named at the bottom of this comment.
//
// Key-pair JWT rather than a password. Not a preference: /api/v2/statements
// accepts OAuth and KEYPAIR_JWT and does not accept a password at all, and
// Snowflake is retiring single-factor password sign-in besides. So there is
// no password option here and no flag to add one. An operator who wants
// their existing OAuth flow can hand a token straight through.
//
// The private key never leaves this struct. It is parsed once at
// construction, held as a *rsa.PrivateKey, and never rendered, logged or
// put into an error. The PEM bytes the caller passed are zeroed after
// parsing: they came from a file or an environment variable and there is no
// reason for a second copy to sit in the heap for the life of the process.
//
// What the REST API costs, stated rather than discovered: a result set
// arrives in JSON partitions and is slower to decode than Arrow, there is
// no session state between statements, and a very large result is paged
// rather than streamed. For a semantic layer returning grouped aggregates
// that is the right trade. For a caller pulling a million detail rows it is
// not, and that caller should be told to use the warehouse directly.
type Snowflake struct {
	opts   SnowflakeOptions
	key    *rsa.PrivateKey
	client *http.Client
	// base is the statements endpoint, built once.
	base string
	// fingerprint is the public key fingerprint the JWT issuer claim needs.
	// Computed once: it is a hash of a key that does not change.
	fingerprint string

	// mu guards the cached token. A JWT is valid for an hour and minting one
	// is an RSA signature, so it is reused rather than signed per query.
	mu        sync.Mutex
	token     string
	tokenGood time.Time
}

// SnowflakeOptions configure the executor.
//
// No password, and no field that holds one. See the type comment.
type SnowflakeOptions struct {
	// Account is the account identifier, for example "ab12345.us-east-1" or
	// an organisation-qualified "myorg-myaccount". Required.
	Account string
	// User is the Snowflake user the key belongs to. Required.
	User string
	// PrivateKeyPEM is an unencrypted or encrypted PKCS#8 private key, read
	// by the caller from a file or an environment variable. Required unless
	// OAuthToken is set.
	//
	// The slice is zeroed by NewSnowflake once the key is parsed.
	PrivateKeyPEM []byte
	// PrivateKeyPassphrase decrypts an encrypted PKCS#8 key. Empty for an
	// unencrypted one.
	PrivateKeyPassphrase string
	// OAuthToken is an alternative to the key pair, for a deployment that
	// already has an OAuth integration. When set, no key is parsed and the
	// token is sent as-is.
	OAuthToken string

	// Warehouse, Database, Schema and Role set the execution context. A
	// statement carries its own fully qualified names, so Database and
	// Schema mostly matter for error messages; Warehouse is required by
	// Snowflake for anything that scans data.
	Warehouse string
	Database  string
	Schema    string
	Role      string

	// AssumeCallerRole runs each statement under a Snowflake role named for
	// the caller rather than under Role, so the warehouse's own grants apply
	// to them.
	//
	// Off by default for the same reason as Postgres: it only works where a
	// role exists per caller, and turning it on without those roles refuses
	// every query.
	AssumeCallerRole bool
	// RequireCallerRole refuses a caller with no derivable role instead of
	// falling back to Role. Without it, a caller Snowflake does not know is
	// served with the service account's own grants, which is the quiet
	// version of no access control at all.
	RequireCallerRole bool

	// Timeout bounds one statement. Zero uses DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the default, for a deployment behind a proxy and
	// for tests. Nil uses a client with the timeout above.
	HTTPClient *http.Client
	// Endpoint overrides the derived https://<account>.snowflakecomputing.com
	// host. Only for tests and for a private-link hostname.
	Endpoint string
}

// NewSnowflake parses the credential and builds the executor.
//
// Nothing is sent at construction. Unlike the Postgres executor there is no
// connection to open: the REST API is stateless, so the first sign that a
// credential is wrong is the first query. A cheap validation statement at
// startup would cost a warehouse resume, which is billable, so it is left
// to the operator to run one.
func NewSnowflake(opts SnowflakeOptions) (*Snowflake, error) {
	if strings.TrimSpace(opts.Account) == "" {
		return nil, errors.New("a Snowflake account identifier is required")
	}
	if strings.TrimSpace(opts.User) == "" {
		return nil, errors.New("a Snowflake user is required")
	}
	if len(opts.PrivateKeyPEM) == 0 && opts.OAuthToken == "" {
		return nil, errors.New(
			"a Snowflake credential is required: either a key-pair private key, " +
				"read from a file or an environment variable, or an OAuth token. " +
				"The SQL REST API does not accept a password")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}

	s := &Snowflake{opts: opts, client: opts.HTTPClient}
	if s.client == nil {
		// The per-request context carries the real deadline; this is a
		// backstop so a hung TLS handshake cannot hold a goroutine forever.
		s.client = &http.Client{Timeout: opts.Timeout + 30*time.Second}
	}

	host := opts.Endpoint
	if host == "" {
		host = "https://" + strings.ToLower(opts.Account) + ".snowflakecomputing.com"
	}
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("snowflake endpoint %q: %w", host, err)
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		// The JWT is a bearer credential. Sending it over plaintext to
		// anything but a loopback test server hands it to the network.
		return nil, fmt.Errorf("snowflake endpoint %q is not https", host)
	}
	s.base = strings.TrimSuffix(parsed.String(), "/") + "/api/v2/statements"

	if opts.OAuthToken == "" {
		key, err := parsePrivateKey(opts.PrivateKeyPEM, opts.PrivateKeyPassphrase)
		if err != nil {
			// Deliberately says nothing about the key's contents.
			return nil, fmt.Errorf("reading the Snowflake private key: %w", err)
		}
		s.key = key
		s.fingerprint, err = publicKeyFingerprint(key)
		if err != nil {
			return nil, err
		}
		// The caller's copy is no longer needed. Theirs may still be
		// referenced elsewhere, but this one is ours to clear.
		for i := range s.opts.PrivateKeyPEM {
			s.opts.PrivateKeyPEM[i] = 0
		}
		s.opts.PrivateKeyPEM = nil
	}
	return s, nil
}

// Name says what this is and that it is unproven.
//
// The dialect emitted golden-tested Snowflake SQL for four releases before
// anything could run it. Saying so here means an operator reading
// /v1/health learns it from the product rather than from a support thread.
func (s *Snowflake) Name() string {
	return "snowflake (SQL REST API, unverified against a live account)"
}

// Close releases nothing: the REST API is stateless and http.Client pools
// its own connections. Present because Executor requires it.
func (s *Snowflake) Close() error {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
	return nil
}

// statementRequest is the body of POST /api/v2/statements.
type statementRequest struct {
	Statement string                      `json:"statement"`
	Timeout   int                         `json:"timeout,omitempty"`
	Database  string                      `json:"database,omitempty"`
	Schema    string                      `json:"schema,omitempty"`
	Warehouse string                      `json:"warehouse,omitempty"`
	Role      string                      `json:"role,omitempty"`
	Bindings  map[string]statementBinding `json:"bindings,omitempty"`
}

type statementBinding struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type statementResponse struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	StatementHandle   string `json:"statementHandle"`
	SQLState          string `json:"sqlState"`
	ResultSetMetaData struct {
		NumRows   int64 `json:"numRows"`
		Format    string
		RowType   []snowflakeColumn `json:"rowType"`
		Partition []struct {
			RowCount         int64 `json:"rowCount"`
			UncompressedSize int64 `json:"uncompressedSize"`
		} `json:"partitionInfo"`
	} `json:"resultSetMetaData"`
	Data [][]*string `json:"data"`
	// Stats is only present on some responses and is the only place
	// Snowflake reports what the statement scanned.
	Stats struct {
		NumRowsInserted int64 `json:"numRowsInserted"`
	} `json:"stats"`
}

type snowflakeColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Scale    *int   `json:"scale"`
	Nullable bool   `json:"nullable"`
}

// Execute runs one statement and returns every partition of its result.
func (s *Snowflake) Execute(ctx context.Context, id govern.Identity, sql string, params []any) (*engine.Rows, error) {
	role, err := s.roleFor(id)
	if err != nil {
		return nil, err
	}

	bindings, err := snowflakeBindings(params)
	if err != nil {
		return nil, err
	}

	body := statementRequest{
		Statement: sql,
		// Snowflake's own server-side bound, in seconds, so a runaway
		// statement stops costing money even if this process goes away.
		Timeout:   int(s.opts.Timeout / time.Second),
		Database:  s.opts.Database,
		Schema:    s.opts.Schema,
		Warehouse: s.opts.Warehouse,
		Role:      role,
		Bindings:  bindings,
	}

	first, err := s.post(ctx, body)
	if err != nil {
		return nil, err
	}

	columns := make([]string, len(first.ResultSetMetaData.RowType))
	for i, c := range first.ResultSetMetaData.RowType {
		columns[i] = c.Name
	}

	out := &engine.Rows{Columns: columns, JobID: first.StatementHandle}
	rows, err := decodeSnowflakeRows(first.ResultSetMetaData.RowType, first.Data)
	if err != nil {
		return nil, err
	}
	out.Rows = rows

	// Partitions after the first are fetched by index. Snowflake returns the
	// first inline and the rest on demand, and a caller that stopped at the
	// first would silently get a prefix of their answer.
	for partition := 1; partition < len(first.ResultSetMetaData.Partition); partition++ {
		more, err := s.partition(ctx, first.StatementHandle, partition)
		if err != nil {
			return nil, fmt.Errorf("reading partition %d of %d: %w",
				partition+1, len(first.ResultSetMetaData.Partition), err)
		}
		rows, err := decodeSnowflakeRows(first.ResultSetMetaData.RowType, more.Data)
		if err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, rows...)
	}

	if want := first.ResultSetMetaData.NumRows; want > 0 && int64(len(out.Rows)) != want {
		// A short read is the failure mode this partition loop exists to
		// prevent, and a wrong total is exactly the class of bug this whole
		// engine is built to refuse. Better to fail than to return a subset
		// that looks like an answer.
		return nil, fmt.Errorf(
			"snowflake reported %d rows and %d arrived; the result was truncated",
			want, len(out.Rows))
	}
	return out, nil
}

// roleFor picks the Snowflake role a statement runs under.
func (s *Snowflake) roleFor(id govern.Identity) (string, error) {
	if !s.opts.AssumeCallerRole {
		return s.opts.Role, nil
	}
	role := snowflakeRoleName(id.Subject)
	if role == "" {
		if s.opts.RequireCallerRole {
			return "", fmt.Errorf(
				"no Snowflake role can be derived for this caller, and "+
					"-impersonate-strict refuses to run their query as %q instead",
				s.opts.Role)
		}
		return s.opts.Role, nil
	}
	return role, nil
}

// snowflakeRoleName turns a subject into a legal unquoted Snowflake
// identifier, or reports empty when it cannot.
//
// Allow-listed rather than escaped. The role name goes into a JSON field
// that Snowflake interpolates into a USE ROLE, so a subject containing a
// quote or a semicolon must not become a role name at all. Refusing is the
// only safe answer: an escaping scheme here would be one bug away from
// letting a subject choose its own privileges.
func snowflakeRoleName(subject string) string {
	// An email local part is the common case: alice@example.com becomes
	// ALICE. Anything outside letters, digits and underscore ends the name.
	local, _, _ := strings.Cut(subject, "@")
	if local == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			return ""
		}
	}
	name := b.String()
	// A Snowflake identifier cannot start with a digit unquoted.
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return ""
	}
	return name
}

// snowflakeBindings renders parameters as Snowflake's positional bindings.
//
// Values are bound, never interpolated. Every type the planner produces is
// listed; an unknown one is an error rather than a fmt.Sprint, because
// fmt.Sprint of an unexpected type is how a value becomes SQL.
func snowflakeBindings(params []any) (map[string]statementBinding, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]statementBinding, len(params))
	for i, v := range params {
		key := strconv.Itoa(i + 1)
		switch value := v.(type) {
		case nil:
			out[key] = statementBinding{Type: "TEXT", Value: nil}
		case string:
			out[key] = statementBinding{Type: "TEXT", Value: value}
		case bool:
			out[key] = statementBinding{Type: "BOOLEAN", Value: strconv.FormatBool(value)}
		case int:
			out[key] = statementBinding{Type: "FIXED", Value: strconv.Itoa(value)}
		case int64:
			out[key] = statementBinding{Type: "FIXED", Value: strconv.FormatInt(value, 10)}
		case float64:
			out[key] = statementBinding{Type: "REAL", Value: strconv.FormatFloat(value, 'f', -1, 64)}
		case *big.Rat:
			out[key] = statementBinding{Type: "REAL", Value: value.FloatString(9)}
		case time.Time:
			out[key] = statementBinding{Type: "TEXT", Value: value.UTC().Format(time.RFC3339Nano)}
		case plan.Date:
			// A DATE, not a timestamp at midnight. Snowflake's TEXT binding
			// against a DATE column coerces by format, and yyyy-mm-dd is
			// the one spelling it reads the same way under every session
			// DATE_INPUT_FORMAT.
			out[key] = statementBinding{Type: "TEXT", Value: value.String()}
		default:
			return nil, fmt.Errorf(
				"cannot bind a %T as a Snowflake parameter; rendering it would "+
					"turn a value into SQL", v)
		}
	}
	return out, nil
}

// post sends a statement and waits for it to finish.
//
// Snowflake answers 200 when the statement completed inside its own
// synchronous window and 202 when it did not. A 202 is polled rather than
// treated as a result: returning an empty body on 202 would answer a
// long-running query with no rows.
func (s *Snowflake) post(ctx context.Context, body statementRequest) (*statementResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatementHandle == "" {
		return resp, nil
	}
	return s.await(ctx, resp)
}

// await polls an accepted statement until it completes or the context ends.
func (s *Snowflake) await(ctx context.Context, resp *statementResponse) (*statementResponse, error) {
	// 090001 is "statement still executing". Anything else on a 202 path is
	// a finished statement.
	const stillRunning = "333334"
	if resp.Code != stillRunning {
		return resp, nil
	}

	// A fixed poll rather than a backoff. The statement is already running
	// on the warehouse; the only cost here is one small request a second,
	// and a growing backoff would add latency to exactly the queries a
	// caller is already waiting on.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("snowflake statement %s did not finish: %w",
				resp.StatementHandle, ctx.Err())
		case <-ticker.C:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			s.base+"/"+url.PathEscape(resp.StatementHandle), nil)
		if err != nil {
			return nil, err
		}
		next, err := s.do(req)
		if err != nil {
			return nil, err
		}
		if next.Code != stillRunning {
			return next, nil
		}
	}
}

func (s *Snowflake) partition(ctx context.Context, handle string, n int) (*statementResponse, error) {
	endpoint := s.base + "/" + url.PathEscape(handle) + "?partition=" + strconv.Itoa(n)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return s.do(req)
}

// do signs, sends and decodes one request.
func (s *Snowflake) do(req *http.Request) (*statementResponse, error) {
	token, kind, err := s.authorization()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Snowflake-Authorization-Token-Type", kind)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "truegrain")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Snowflake: %w", err)
	}
	defer resp.Body.Close()

	// Bounded, because a malformed or hostile response should not be able to
	// exhaust this process's memory. 256 MiB is far above any partition
	// Snowflake returns and far below a problem.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the Snowflake response: %w", err)
	}

	var decoded statementResponse
	// A non-JSON body happens on a proxy error page, and json's message
	// about it is less useful than the status.
	if jsonErr := json.Unmarshal(payload, &decoded); jsonErr != nil {
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			return nil, &snowflakeError{status: resp.StatusCode,
				message: strings.TrimSpace(firstLine(string(payload)))}
		}
		return nil, fmt.Errorf("the Snowflake response is not JSON: %w", jsonErr)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
		return &decoded, nil
	default:
		return nil, &snowflakeError{
			status:  resp.StatusCode,
			code:    decoded.Code,
			message: decoded.Message,
			handle:  decoded.StatementHandle,
		}
	}
}

// authorization returns the bearer token and the type header Snowflake needs
// to interpret it.
func (s *Snowflake) authorization() (token, kind string, err error) {
	if s.opts.OAuthToken != "" {
		return s.opts.OAuthToken, "OAUTH", nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-minted a minute early so a token cannot expire in flight.
	if s.token != "" && time.Now().Before(s.tokenGood.Add(-time.Minute)) {
		return s.token, "KEYPAIR_JWT", nil
	}

	// An hour is Snowflake's maximum lifetime for this JWT.
	const lifetime = time.Hour
	issued := time.Now()
	signed, err := s.mintJWT(issued, issued.Add(lifetime))
	if err != nil {
		return "", "", err
	}
	s.token, s.tokenGood = signed, issued.Add(lifetime)
	return s.token, "KEYPAIR_JWT", nil
}

// mintJWT builds and signs the RS256 assertion Snowflake expects.
//
// Hand-rolled rather than pulled from a JWT library, because the whole of it
// is one JSON header, one JSON claim set and an RSA-SHA256 signature over
// their base64url concatenation. A library would add a dependency to save
// twenty lines and would still need this issuer format written out.
func (s *Snowflake) mintJWT(issued, expires time.Time) (string, error) {
	subject := strings.ToUpper(snowflakeAccountName(s.opts.Account)) + "." +
		strings.ToUpper(s.opts.User)

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": subject + "." + s.fingerprint,
		"sub": subject,
		"iat": issued.Unix(),
		"exp": expires.Unix(),
	})
	if err != nil {
		return "", err
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing the Snowflake assertion: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(signature), nil
}

// snowflakeAccountName strips the region and cloud from a legacy account
// identifier, which is what the JWT issuer claim wants.
//
// "ab12345.us-east-1.aws" is the account ab12345. An organisation-qualified
// identifier like "myorg-myaccount" has no dot and passes through.
func snowflakeAccountName(account string) string {
	name, _, _ := strings.Cut(account, ".")
	return name
}

// publicKeyFingerprint is SHA256:<base64 of the SHA-256 of the DER SPKI>,
// which is the form Snowflake prints from DESCRIBE USER and the form the
// issuer claim must match exactly.
func publicKeyFingerprint(key *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("encoding the public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// parsePrivateKey reads a PKCS#8 key, encrypted or not.
//
// PKCS#1 is accepted too because an operator who generated a key with an
// older openssl has one, and refusing it would be a puzzle rather than a
// policy. An encrypted PKCS#1 key, the kind with DEK-Info headers, is
// refused: Go's decryption for that format is deprecated and broken by
// design, and telling somebody to convert the key is better than decrypting
// it badly.
func parsePrivateKey(pemBytes []byte, passphrase string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found; expected a BEGIN PRIVATE KEY header")
	}

	if _, encrypted := block.Headers["DEK-Info"]; encrypted {
		return nil, errors.New(
			"this key is in the legacy encrypted PEM format, which cannot be " +
				"read safely. Convert it with: openssl pkcs8 -topk8 -in key.pem -out key.p8")
	}

	var parsed any
	var err error
	switch {
	case block.Type == "ENCRYPTED PRIVATE KEY":
		if passphrase == "" {
			return nil, errors.New("the key is encrypted and no passphrase was given")
		}
		// Go's standard library cannot read an encrypted PKCS#8 key, and
		// there is no safe short version of the algorithm. Saying so is
		// better than a dependency added for one branch.
		return nil, errors.New(
			"an encrypted PKCS#8 key is not supported. Decrypt it once into a " +
				"file the process can read, or hold the decrypted key in the " +
				"platform secret manager: openssl pkcs8 -in key.p8 -out key.pem")
	case block.Type == "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, err
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is a %T; Snowflake key-pair authentication requires RSA", parsed)
	}
	return key, nil
}

// decodeSnowflakeRows turns the JSON text Snowflake returns into typed
// values.
//
// Every value arrives as a string or null, so the column metadata is the
// only source of type. Getting this wrong is the bug the Postgres wire
// surface already produced once: a measure that arrives as text makes a BI
// tool file it as a category, and a number rendered through a float loses
// the scale that made it money.
func decodeSnowflakeRows(columns []snowflakeColumn, data [][]*string) ([][]any, error) {
	out := make([][]any, 0, len(data))
	for _, row := range data {
		if len(row) != len(columns) {
			return nil, fmt.Errorf("snowflake returned %d values for %d columns",
				len(row), len(columns))
		}
		decoded := make([]any, len(row))
		for i, cell := range row {
			if cell == nil {
				decoded[i] = nil
				continue
			}
			value, err := decodeSnowflakeCell(columns[i], *cell)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", columns[i].Name, err)
			}
			decoded[i] = value
		}
		out = append(out, decoded)
	}
	return out, nil
}

func decodeSnowflakeCell(column snowflakeColumn, text string) (any, error) {
	switch strings.ToUpper(column.Type) {
	case "FIXED":
		// Scale zero is an integer. Anything else is a decimal, and a
		// decimal is kept exact: this is the money path, and a float64
		// turns 885.50 into 885.5 and eventually into 885.4999999.
		if column.Scale == nil || *column.Scale == 0 {
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				// Wider than int64. Keeping it as a rational is lossless and
				// renders the same.
				return exactDecimal(text)
			}
			return n, nil
		}
		return exactDecimal(text)
	case "REAL", "FLOAT", "DOUBLE":
		return strconv.ParseFloat(text, 64)
	case "BOOLEAN":
		return strconv.ParseBool(text)
	case "DATE":
		// Snowflake returns a date as days since the epoch, not as a string.
		days, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a DATE arrived as %q, which is not a day count", text)
		}
		return time.Unix(0, 0).UTC().AddDate(0, 0, int(days)), nil
	case "TIMESTAMP_NTZ", "TIMESTAMP_LTZ", "TIMESTAMP_TZ", "TIME":
		return snowflakeEpoch(text)
	default:
		// TEXT, VARIANT, OBJECT, ARRAY, BINARY and anything Snowflake adds
		// later. A string is what arrived and a string is what it stays.
		return text, nil
	}
}

// exactDecimal keeps a decimal exact rather than rounding it into a float.
func exactDecimal(text string) (any, error) {
	rat, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("%q is not a number", text)
	}
	return rat, nil
}

// snowflakeEpoch reads the fractional seconds-since-epoch Snowflake uses for
// its timestamp types, for example "1735689600.123456789".
func snowflakeEpoch(text string) (time.Time, error) {
	whole, fraction, _ := strings.Cut(text, ".")
	seconds, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("a timestamp arrived as %q", text)
	}
	var nanos int64
	if fraction != "" {
		// Right-pad to nanosecond precision so ".5" is half a second rather
		// than five nanoseconds.
		for len(fraction) < 9 {
			fraction += "0"
		}
		nanos, err = strconv.ParseInt(fraction[:9], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("a timestamp arrived as %q", text)
		}
	}
	return time.Unix(seconds, nanos).UTC(), nil
}

// snowflakeError carries what Snowflake said, without the credential.
type snowflakeError struct {
	status  int
	code    string
	message string
	handle  string
}

func (e *snowflakeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "snowflake returned %d", e.status)
	if e.code != "" {
		fmt.Fprintf(&b, " (%s)", e.code)
	}
	if e.message != "" {
		fmt.Fprintf(&b, ": %s", e.message)
	}
	if e.handle != "" {
		fmt.Fprintf(&b, " [statement %s]", e.handle)
	}
	return b.String()
}

// Transient classifies a failure that retrying the identical statement might
// survive.
//
// An allow-list, like every other executor here. The default is not
// transient: retrying a permission denial three times is noise, and
// retrying a statement that already ran costs money twice.
func (s *Snowflake) Transient(err error) bool {
	var sf *snowflakeError
	if errors.As(err, &sf) {
		switch sf.status {
		case http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}

	// A connection that failed to establish never reached the warehouse, so
	// the statement did not run. Anything after that is ambiguous and is
	// left alone.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 200 {
		return line[:200]
	}
	return line
}
