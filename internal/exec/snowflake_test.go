package exec_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/exec"
	"github.com/rk-chavali/truegrain/internal/govern"
	"github.com/rk-chavali/truegrain/internal/plan"
)

// The Snowflake executor, tested against a stand-in for the SQL REST API.
//
// Nothing here proves it works against Snowflake, and no test in this
// repository can: that needs an account, a warehouse and a key registered
// on a user. What these do prove is the half that is this engine's fault
// when it goes wrong. The assertion Snowflake will reject, the parameter
// that became SQL, the decimal that lost its scale, the partition that was
// never fetched: all of those are bugs on this side of the wire and all of
// them are visible from here.
//
// The other half, that Snowflake accepts what is sent, is stated as
// unverified in Name() rather than implied by a green test run.

func snowflakeKey(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	// 2048 rather than 4096: this is a test key, and the signature path is
	// identical. Generation dominates the runtime of every test here.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), key
}

// fakeSnowflake serves one canned response and records what it was sent.
type fakeSnowflake struct {
	*httptest.Server
	requests []recordedRequest
	// respond returns the status and body for request n.
	respond func(n int, r *http.Request) (int, string)
}

type recordedRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   map[string]any
}

func newFakeSnowflake(t *testing.T, respond func(n int, r *http.Request) (int, string)) *fakeSnowflake {
	t.Helper()
	f := &fakeSnowflake{respond: respond}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			header: r.Header.Clone(),
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&rec.body)
		}
		n := len(f.requests)
		f.requests = append(f.requests, rec)

		status, body := f.respond(n, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func snowflakeWith(t *testing.T, f *fakeSnowflake) (*exec.Snowflake, *rsa.PrivateKey) {
	t.Helper()
	pemBytes, key := snowflakeKey(t)
	sf, err := exec.NewSnowflake(exec.SnowflakeOptions{
		Account:       "ab12345.us-east-1",
		User:          "svc_truegrain",
		PrivateKeyPEM: pemBytes,
		Warehouse:     "COMPUTE_WH",
		Database:      "ANALYTICS",
		Schema:        "MAIN",
		Role:          "TRUEGRAIN",
		Endpoint:      f.URL,
		Timeout:       10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sf, key
}

// okResponse is a well-formed completed statement.
func okResponse(rowType, data string) string {
	return fmt.Sprintf(`{
		"code": "090001",
		"statementHandle": "01b2-0000-handle",
		"resultSetMetaData": {"numRows": %d, "rowType": [%s], "partitionInfo": [{"rowCount": %d}]},
		"data": [%s]
	}`, strings.Count(data, "[")-strings.Count(data, "[[")*0, rowType, 1, data)
}

// TestTheAssertionIsAValidRS256JWTForThisKey.
//
// Snowflake will reject a malformed assertion with a message about
// authentication, which reads as a wrong key rather than as a wrong
// signature, and an operator can lose a day to that. The signature, the
// issuer format and the lifetime are all checkable here.
func TestTheAssertionIsAValidRS256JWTForThisKey(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, okResponse(`{"name":"N","type":"FIXED","scale":0}`, `["1"]`)
	})
	sf, key := snowflakeWith(t, f)

	if _, err := sf.Execute(context.Background(),
		govern.Identity{Subject: "tester"}, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}

	auth := f.requests[0].header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q", auth)
	}
	if got := f.requests[0].header.Get("X-Snowflake-Authorization-Token-Type"); got != "KEYPAIR_JWT" {
		t.Errorf("token type header = %q; Snowflake reads the assertion as OAuth without it", got)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("the assertion is not a JWT: %q", token)
	}
	signingInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the assertion does not verify against its own key: %v", err)
	}

	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}

	// The subject is the account without its region, uppercased. Leaving the
	// region on is the single most common key-pair setup mistake and
	// Snowflake's error for it says only "JWT token is invalid".
	const want = "AB12345.SVC_TRUEGRAIN"
	if claims.Sub != want {
		t.Errorf("sub = %q, want %q", claims.Sub, want)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	wantIss := want + ".SHA256:" + base64.StdEncoding.EncodeToString(sum[:])
	if claims.Iss != wantIss {
		t.Errorf("iss = %q\nwant %q", claims.Iss, wantIss)
	}

	// An hour is Snowflake's maximum. A longer one is rejected outright.
	if lifetime := claims.Exp - claims.Iat; lifetime <= 0 || lifetime > 3600 {
		t.Errorf("the assertion lives for %ds; Snowflake accepts up to 3600", lifetime)
	}
}

// TestTheAssertionIsReusedRatherThanResigned, because an RSA signature per
// query is pure cost on a surface whose whole point is being cheap.
func TestTheAssertionIsReusedRatherThanResigned(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, okResponse(`{"name":"N","type":"FIXED","scale":0}`, `["1"]`)
	})
	sf, _ := snowflakeWith(t, f)

	for range 3 {
		if _, err := sf.Execute(context.Background(),
			govern.Identity{Subject: "tester"}, "SELECT 1", nil); err != nil {
			t.Fatal(err)
		}
	}
	first := f.requests[0].header.Get("Authorization")
	for i, r := range f.requests {
		if got := r.header.Get("Authorization"); got != first {
			t.Errorf("request %d minted a second assertion", i)
		}
	}
}

// TestParametersAreBoundNeverInterpolated.
//
// The property the whole engine rests on. The statement Snowflake receives
// must be the statement the emitter produced, with the values alongside it.
func TestParametersAreBoundNeverInterpolated(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, okResponse(`{"name":"N","type":"FIXED","scale":0}`, `["1"]`)
	})
	sf, _ := snowflakeWith(t, f)

	const hostile = "'; DROP TABLE orders; --"
	sql := `SELECT SUM("x") FROM "t" WHERE "region" = ? AND "d" = ?`
	if _, err := sf.Execute(context.Background(), govern.Identity{Subject: "tester"},
		sql, []any{hostile, plan.Date{Year: 2026, Month: time.February, Day: 1}}); err != nil {
		t.Fatal(err)
	}

	sent, _ := f.requests[0].body["statement"].(string)
	if sent != sql {
		t.Fatalf("the statement was rewritten:\n got %q\nwant %q", sent, sql)
	}
	if strings.Contains(sent, "DROP") {
		t.Fatal("a parameter reached the statement text")
	}

	bindings, _ := f.requests[0].body["bindings"].(map[string]any)
	if len(bindings) != 2 {
		t.Fatalf("bindings = %v", bindings)
	}
	one, _ := bindings["1"].(map[string]any)
	if one["value"] != hostile {
		t.Errorf("binding 1 = %v, want the value unchanged", one["value"])
	}
	two, _ := bindings["2"].(map[string]any)
	if two["value"] != "2026-02-01" {
		t.Errorf("a date bound as %v; Snowflake reads yyyy-mm-dd under every "+
			"session DATE_INPUT_FORMAT", two["value"])
	}
}

// TestAnUnbindableTypeIsRefusedRatherThanRendered.
//
// The gap every executor has to close. A default branch that reached for
// fmt.Sprint would turn an unexpected type into text and that text into
// SQL, which is the one path from a value to a statement.
func TestAnUnbindableTypeIsRefusedRatherThanRendered(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, okResponse(`{"name":"N","type":"FIXED","scale":0}`, `["1"]`)
	})
	sf, _ := snowflakeWith(t, f)

	_, err := sf.Execute(context.Background(), govern.Identity{Subject: "tester"},
		"SELECT ?", []any{struct{ Evil string }{"'; DROP TABLE orders; --"}})
	if err == nil {
		t.Fatal("an arbitrary struct was accepted as a parameter")
	}
	if !strings.Contains(err.Error(), "cannot bind") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// TestNumbersKeepTheirScale.
//
// The failure this engine exists to prevent, one layer down. A decimal read
// through float64 turns 885.50 into 885.5 and then, after enough
// arithmetic, into something that is not 885.50 at all. Money is FIXED with
// a scale in Snowflake and has to stay exact.
func TestNumbersKeepTheirScale(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, `{
			"code": "090001", "statementHandle": "h",
			"resultSetMetaData": {"numRows": 1, "partitionInfo": [{"rowCount": 1}], "rowType": [
				{"name": "revenue", "type": "FIXED", "scale": 2},
				{"name": "orders",  "type": "FIXED", "scale": 0},
				{"name": "ratio",   "type": "REAL"},
				{"name": "region",  "type": "TEXT"},
				{"name": "active",  "type": "BOOLEAN"},
				{"name": "day",     "type": "DATE"},
				{"name": "seen",    "type": "TIMESTAMP_NTZ"},
				{"name": "missing", "type": "TEXT"}
			]},
			"data": [["885.50", "5", "0.25", "North East", "true", "20454", "1767225600.500000000", null]]
		}`
	})
	sf, _ := snowflakeWith(t, f)

	rows, err := sf.Execute(context.Background(), govern.Identity{Subject: "tester"}, "SELECT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 {
		t.Fatalf("rows = %v", rows.Rows)
	}
	row := rows.Rows[0]

	revenue, ok := row[0].(*big.Rat)
	if !ok {
		t.Fatalf("a scaled decimal arrived as %T, which cannot represent 885.50 exactly", row[0])
	}
	if got := revenue.FloatString(2); got != "885.50" {
		t.Errorf("revenue = %s, want 885.50", got)
	}
	if orders, ok := row[1].(int64); !ok || orders != 5 {
		t.Errorf("a scale-zero FIXED arrived as %T %v, want int64 5", row[1], row[1])
	}
	if ratio, ok := row[2].(float64); !ok || ratio != 0.25 {
		t.Errorf("ratio = %T %v", row[2], row[2])
	}
	if region, ok := row[3].(string); !ok || region != "North East" {
		t.Errorf("region = %T %v", row[3], row[3])
	}
	if active, ok := row[4].(bool); !ok || !active {
		t.Errorf("active = %T %v", row[4], row[4])
	}
	day, ok := row[5].(time.Time)
	if !ok || day.Format("2006-01-02") != "2026-01-01" {
		t.Errorf("a DATE arrived as %T %v; Snowflake sends a day count, not text", row[5], row[5])
	}
	seen, ok := row[6].(time.Time)
	if !ok || seen.UTC().Format("2006-01-02T15:04:05.000") != "2026-01-01T00:00:00.500" {
		t.Errorf("a timestamp arrived as %T %v", row[6], row[6])
	}
	if row[7] != nil {
		t.Errorf("a null arrived as %T %v", row[7], row[7])
	}
}

// TestEveryPartitionIsFetched.
//
// Snowflake returns the first partition inline and the rest by index. An
// executor that stopped at the first would return a prefix of the answer
// under the right column headers, which is the exact failure mode this
// product refuses everywhere else.
func TestEveryPartitionIsFetched(t *testing.T) {
	f := newFakeSnowflake(t, func(n int, r *http.Request) (int, string) {
		const meta = `"resultSetMetaData": {"numRows": 3, "rowType": [{"name":"n","type":"TEXT"}],
			"partitionInfo": [{"rowCount":1},{"rowCount":1},{"rowCount":1}]}`
		switch n {
		case 0:
			return 200, `{"code":"090001","statementHandle":"h",` + meta + `,"data":[["a"]]}`
		case 1:
			return 200, `{"code":"090001","statementHandle":"h","data":[["b"]]}`
		default:
			return 200, `{"code":"090001","statementHandle":"h","data":[["c"]]}`
		}
	})
	sf, _ := snowflakeWith(t, f)

	rows, err := sf.Execute(context.Background(), govern.Identity{Subject: "tester"}, "SELECT n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 3 {
		t.Fatalf("want 3 rows across 3 partitions, got %d: %v", len(rows.Rows), rows.Rows)
	}
	if len(f.requests) != 3 {
		t.Fatalf("want 3 requests, got %d", len(f.requests))
	}
	for i, want := range []string{"partition=1", "partition=2"} {
		if got := f.requests[i+1].query; got != want {
			t.Errorf("request %d asked for %q, want %q", i+1, got, want)
		}
	}
}

// TestAShortResultIsAnErrorNotAnAnswer.
//
// If Snowflake says three rows and two arrive, something between here and
// there dropped one. Returning the two would be a wrong total that looks
// exactly like a right one.
func TestAShortResultIsAnErrorNotAnAnswer(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, `{"code":"090001","statementHandle":"h",
			"resultSetMetaData": {"numRows": 3, "rowType": [{"name":"n","type":"TEXT"}],
				"partitionInfo": [{"rowCount":3}]},
			"data": [["a"],["b"]]}`
	})
	sf, _ := snowflakeWith(t, f)

	_, err := sf.Execute(context.Background(), govern.Identity{Subject: "tester"}, "SELECT n", nil)
	if err == nil {
		t.Fatal("a truncated result was returned as an answer")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("the error does not say what happened: %v", err)
	}
}

// TestAnAcceptedStatementIsPolledToCompletion, because a 202 with an empty
// body is what a long-running query looks like and answering it with no
// rows would be a zero where the total should be.
func TestAnAcceptedStatementIsPolledToCompletion(t *testing.T) {
	f := newFakeSnowflake(t, func(n int, r *http.Request) (int, string) {
		if n == 0 {
			return 202, `{"code":"333334","statementHandle":"h","message":"Asynchronous execution in progress"}`
		}
		return 200, `{"code":"090001","statementHandle":"h",
			"resultSetMetaData": {"numRows": 1, "rowType": [{"name":"n","type":"TEXT"}],
				"partitionInfo": [{"rowCount":1}]},
			"data": [["done"]]}`
	})
	sf, _ := snowflakeWith(t, f)

	rows, err := sf.Execute(context.Background(), govern.Identity{Subject: "tester"}, "SELECT n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0][0] != "done" {
		t.Errorf("the poll did not return the finished result: %v", rows.Rows)
	}
	if f.requests[1].method != http.MethodGet || f.requests[1].path != "/api/v2/statements/h" {
		t.Errorf("the poll went to %s %s", f.requests[1].method, f.requests[1].path)
	}
}

// TestAHostileSubjectNeverBecomesARoleName.
//
// With -impersonate the caller's subject picks the Snowflake role. The role
// goes into a field Snowflake turns into USE ROLE, so a subject carrying a
// quote must not become a role name at all. Escaping it would be one bug
// away from a caller choosing their own privileges; refusing is not.
func TestAHostileSubjectNeverBecomesARoleName(t *testing.T) {
	pemBytes, _ := snowflakeKey(t)
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, okResponse(`{"name":"N","type":"TEXT"}`, `["x"]`)
	})
	sf, err := exec.NewSnowflake(exec.SnowflakeOptions{
		Account: "ab12345", User: "svc", PrivateKeyPEM: pemBytes,
		Role: "TRUEGRAIN", AssumeCallerRole: true, Endpoint: f.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, subject := range []string{
		`alice"; USE ROLE ACCOUNTADMIN; --@example.com`,
		`alice' OR '1'='1@example.com`,
		`admin ROLE@example.com`,
		`@example.com`,
	} {
		f.requests = nil
		if _, err := sf.Execute(context.Background(),
			govern.Identity{Subject: subject}, "SELECT 1", nil); err != nil {
			t.Fatal(err)
		}
		role, _ := f.requests[0].body["role"].(string)
		if role != "TRUEGRAIN" {
			t.Errorf("subject %q produced role %q; it should have fallen back to the "+
				"configured role rather than being carried", subject, role)
		}
	}

	// And the ordinary case still derives one, or the guard above is only
	// passing because nothing ever derives a role.
	f.requests = nil
	if _, err := sf.Execute(context.Background(),
		govern.Identity{Subject: "alice@example.com"}, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.requests[0].body["role"].(string); role != "ALICE" {
		t.Errorf("an ordinary subject derived %q, want ALICE", role)
	}
}

// TestStrictImpersonationRefusesRatherThanFallingBack, because falling back
// silently serves an unknown caller with the service account's own grants.
func TestStrictImpersonationRefusesRatherThanFallingBack(t *testing.T) {
	pemBytes, _ := snowflakeKey(t)
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 200, okResponse(`{"name":"N","type":"TEXT"}`, `["x"]`)
	})
	sf, err := exec.NewSnowflake(exec.SnowflakeOptions{
		Account: "ab12345", User: "svc", PrivateKeyPEM: pemBytes,
		Role: "TRUEGRAIN", AssumeCallerRole: true, RequireCallerRole: true,
		Endpoint: f.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = sf.Execute(context.Background(),
		govern.Identity{Subject: `bad"subject`}, "SELECT 1", nil)
	if err == nil {
		t.Fatal("a caller with no derivable role was served anyway")
	}
	if len(f.requests) != 0 {
		t.Error("the statement was sent before the role check")
	}
}

// TestAPlaintextEndpointIsRefused.
//
// The assertion is a bearer credential with an hour of life on it. Sending
// it to an http:// host hands it to anything on the path, and a
// misconfigured endpoint is a plausible mistake rather than an exotic one.
func TestAPlaintextEndpointIsRefused(t *testing.T) {
	pemBytes, _ := snowflakeKey(t)
	_, err := exec.NewSnowflake(exec.SnowflakeOptions{
		Account: "ab12345", User: "svc", PrivateKeyPEM: pemBytes,
		Endpoint: "http://snowflake.example.com",
	})
	if err == nil {
		t.Fatal("a plaintext endpoint was accepted")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// TestTheKeyNeverReachesAnError.
//
// Every error path here is read by somebody pasting it into a ticket. A
// private key that appears in one has been disclosed and has to be rotated,
// and the person pasting it will not know that.
func TestTheKeyNeverReachesAnError(t *testing.T) {
	pemBytes, _ := snowflakeKey(t)
	secret := string(pemBytes)

	// A key that parses, against an endpoint that fails.
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 403, `{"code":"390144","message":"JWT token is invalid"}`
	})
	sf, _ := snowflakeWith(t, f)
	_, err := sf.Execute(context.Background(), govern.Identity{Subject: "t"}, "SELECT 1", nil)
	if err == nil {
		t.Fatal("a 403 was not reported")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), secret[40:80]) {
		t.Error("the private key reached an error message")
	}

	// And a key that does not parse.
	_, err = exec.NewSnowflake(exec.SnowflakeOptions{
		Account: "ab12345", User: "svc",
		PrivateKeyPEM: []byte("-----BEGIN PRIVATE KEY-----\nbm90IGEga2V5\n-----END PRIVATE KEY-----\n"),
	})
	if err == nil {
		t.Fatal("a malformed key was accepted")
	}
	if strings.Contains(err.Error(), "bm90IGEga2V5") {
		t.Errorf("the key material reached the error: %v", err)
	}
}

// TestAnErrorFromSnowflakeCarriesItsCode, because "snowflake returned 400"
// sends an operator to the wrong place and 002003 names a missing object.
func TestAnErrorFromSnowflakeCarriesItsCode(t *testing.T) {
	f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
		return 422, `{"code":"002003","message":"SQL compilation error: Object 'ORDERS' does not exist",
			"statementHandle":"01b2-handle","sqlState":"42S02"}`
	})
	sf, _ := snowflakeWith(t, f)

	_, err := sf.Execute(context.Background(), govern.Identity{Subject: "t"}, "SELECT 1", nil)
	if err == nil {
		t.Fatal("an error response was treated as a result")
	}
	for _, want := range []string{"002003", "does not exist", "01b2-handle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error omits %q: %v", want, err)
		}
	}
}

// TestOnlyAKnownTransientFailureIsRetried.
//
// The classifier is an allow-list on purpose. A retried permission denial
// is noise; a retried statement that already ran is billed twice.
func TestOnlyAKnownTransientFailureIsRetried(t *testing.T) {
	for _, c := range []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusForbidden, false},
		{http.StatusUnauthorized, false},
		{http.StatusBadRequest, false},
		{422, false},
	} {
		status := c.status
		f := newFakeSnowflake(t, func(int, *http.Request) (int, string) {
			return status, `{"code":"x","message":"y"}`
		})
		sf, _ := snowflakeWith(t, f)
		_, err := sf.Execute(context.Background(), govern.Identity{Subject: "t"}, "SELECT 1", nil)
		if err == nil {
			t.Fatalf("%d was not an error", status)
		}
		if got := sf.Transient(err); got != c.want {
			t.Errorf("%d classified transient=%v, want %v", status, got, c.want)
		}
	}
}

// TestNameSaysItIsUnverified.
//
// The dialect emitted golden-tested Snowflake for four releases with
// nothing able to run it. An operator reading /v1/health should learn the
// executor's status from the product rather than from a support thread.
func TestNameSaysItIsUnverified(t *testing.T) {
	pemBytes, _ := snowflakeKey(t)
	sf, err := exec.NewSnowflake(exec.SnowflakeOptions{
		Account: "ab12345", User: "svc", PrivateKeyPEM: pemBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sf.Name(), "unverified") {
		t.Errorf("Name() = %q and does not disclose that no account has run it", sf.Name())
	}
}

// TestACredentialIsRequired, because an executor that constructed without
// one would fail on the first caller's query rather than at startup.
func TestACredentialIsRequired(t *testing.T) {
	_, err := exec.NewSnowflake(exec.SnowflakeOptions{Account: "ab12345", User: "svc"})
	if err == nil {
		t.Fatal("an executor was built with no credential")
	}
	if !strings.Contains(err.Error(), "does not accept a password") {
		t.Errorf("the error does not explain why there is no password option: %v", err)
	}
}
