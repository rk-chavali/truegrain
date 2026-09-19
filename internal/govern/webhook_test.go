package govern_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Telling something else that a decision happened.
//
// The properties worth asserting are the ones that make this safe to put
// on the query path: it never blocks, it never sends more than the audit
// record already contains, and what it does send can be shown to be
// genuine.

type receiver struct {
	mu     sync.Mutex
	bodies []string
	sigs   []string
	status int
	delay  time.Duration
	server *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		delay, status := r.delay, r.status
		r.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.sigs = append(r.sigs, req.Header.Get("X-Truegrain-Signature"))
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func (r *receiver) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.received()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d of %d events arrived", len(r.received()), n)
}

func refusalEvent() govern.Event {
	return govern.Event{
		Time: time.Now().UTC(), Identity: "agent@example.com",
		Decision: "refused", RefusalCode: "fan_out_would_inflate",
		Reason: "joining order_lines repeats every orders row",
		Hint:   "line_revenue answers this at the right grain",
	}
}

// TestARefusalReachesTheEndpoint, which is the point.
func TestARefusalReachesTheEndpoint(t *testing.T) {
	r := newReceiver(t)
	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(refusalEvent())
	r.waitFor(t, 1)

	var got govern.Event
	if err := json.Unmarshal([]byte(r.received()[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.RefusalCode != "fan_out_would_inflate" {
		t.Errorf("code = %q", got.RefusalCode)
	}
	if !strings.Contains(got.Hint, "line_revenue") {
		t.Errorf("the hint did not survive: %q", got.Hint)
	}
}

// TestAnAllowedQueryIsNotShippedByDefault.
//
// Sending every answered query to a third party is a firehose and a
// disclosure nobody asked for. The thing worth being told about is the
// question that could not be answered.
func TestAnAllowedQueryIsNotShippedByDefault(t *testing.T) {
	r := newReceiver(t)
	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(govern.Event{Decision: "allowed", Identity: "agent@example.com"})
	w.Write(refusalEvent())
	r.waitFor(t, 1)
	time.Sleep(150 * time.Millisecond)

	if n := len(r.received()); n != 1 {
		t.Errorf("%d events delivered, want only the refusal", n)
	}
}

// TestTheBodyIsSigned, because the receiver is being told what your
// callers are asking, and an unsigned endpoint is an invitation to feed
// somebody's alerting a fiction.
func TestTheBodyIsSigned(t *testing.T) {
	r := newReceiver(t)
	const secret = "not-a-real-secret"
	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(refusalEvent())
	r.waitFor(t, 1)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(r.received()[0]))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r.mu.Lock()
	got := r.sigs[0]
	r.mu.Unlock()
	if got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
}

// TestASlowReceiverDoesNotBlockTheCaller.
//
// Write is called on the query path. A webhook to somebody else's service
// must never become a slow query, which is the whole reason for the buffer
// and the background sender.
func TestASlowReceiverDoesNotBlockTheCaller(t *testing.T) {
	r := newReceiver(t)
	r.mu.Lock()
	r.delay = 2 * time.Second
	r.mu.Unlock()

	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL, BufferSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	start := time.Now()
	for range 50 {
		w.Write(refusalEvent())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Write blocked for %v against a slow receiver", elapsed)
	}
}

// TestAFullBufferDropsAndCounts.
//
// Blocking the engine to deliver a notification would be the notification
// breaking the thing it reports on, so events are dropped. Dropping
// silently would be worse: the count is what tells an operator their
// endpoint cannot keep up.
func TestAFullBufferDropsAndCounts(t *testing.T) {
	r := newReceiver(t)
	r.mu.Lock()
	r.delay = time.Second
	r.mu.Unlock()

	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL, BufferSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for range 100 {
		w.Write(refusalEvent())
	}
	if _, _, dropped := w.Stats(); dropped == 0 {
		t.Error("100 events into a 2-slot buffer against a slow receiver dropped none")
	}
}

// TestPlainHTTPOffLoopbackIsRefused.
//
// The payload names which identities asked what. On plain http to a third
// party that is readable by anything on the path.
func TestPlainHTTPOffLoopbackIsRefused(t *testing.T) {
	_, err := govern.NewWebhook(govern.WebhookOptions{URL: "http://alerts.example.com/hook"})
	if err == nil {
		t.Fatal("a plaintext webhook to a remote host was accepted")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("the error does not say what to do: %v", err)
	}

	// Loopback is allowed, or local development is impossible.
	r := newReceiver(t)
	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL})
	if err != nil {
		t.Fatalf("a loopback webhook was refused: %v", err)
	}
	_ = w.Close()
}

// TestTheEventCarriesNoValuesOrSQL.
//
// The audit shape is already free of filter values, SQL text and rows, and
// is mutation tested to stay that way. This asserts the webhook does not
// become a second, unguarded disclosure path by enriching it.
func TestTheEventCarriesNoValuesOrSQL(t *testing.T) {
	r := newReceiver(t)
	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(refusalEvent())
	r.waitFor(t, 1)

	var payload map[string]any
	if err := json.Unmarshal([]byte(r.received()[0]), &payload); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"compiled_sql", "sql", "rows", "values", "filters"} {
		if _, present := payload[forbidden]; present {
			t.Errorf("the webhook payload carries %q", forbidden)
		}
	}
}

// TestAFailingReceiverIsCountedNotSilent.
//
// A receiver that is down must be visible, or an operator believes their
// alerting is working when nothing has arrived for a week.
func TestAFailingReceiverIsCountedNotSilent(t *testing.T) {
	r := newReceiver(t)
	r.mu.Lock()
	r.status = http.StatusInternalServerError
	r.mu.Unlock()

	w, err := govern.NewWebhook(govern.WebhookOptions{URL: r.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Write(refusalEvent())
	r.waitFor(t, 1)
	time.Sleep(200 * time.Millisecond)

	sent, failed, _ := w.Stats()
	if failed == 0 {
		t.Error("a rejected delivery was not counted as failed")
	}
	if sent != 0 {
		t.Errorf("a rejected delivery was counted as sent (%d)", sent)
	}
}
