package govern

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Telling something else that a decision happened.
//
// Nothing could subscribe to "a refusal happened", so the only way to know
// an agent was hitting a fan-out repeatedly was for somebody to open the
// audit page and look. That is the wrong shape for the thing most worth
// noticing: a refusal is a signal that a dashboard is asking a question the
// model cannot answer, and it should reach whoever owns the model without
// them going to look.
//
// # Three properties, and why each one is not optional
//
// It never blocks a query. A webhook is an outbound call to somebody else's
// service, and a slow one must not become a slow query. Events go to a
// bounded buffer and a background sender; when the buffer is full, events
// are dropped and the drop is counted, because blocking the engine to
// deliver a notification would be the notification breaking the thing it
// reports on.
//
// It sends the audit event and nothing else. That shape is already free of
// filter values, SQL text and rows, and is mutation tested to stay that
// way. A webhook that enriched the payload would be a second, unguarded
// disclosure path pointed at a third party.
//
// It signs what it sends. The receiver is being told what your callers are
// asking, which is worth knowing is genuine; an unsigned webhook endpoint
// is an open invitation to feed somebody's alerting fiction.

// WebhookOptions configure delivery.
type WebhookOptions struct {
	// URL is where events are posted. https only, unless it is loopback.
	URL string
	// Secret signs the body as X-Truegrain-Signature. Strongly encouraged:
	// without one the receiver cannot tell your engine from anybody who
	// found the endpoint.
	Secret string
	// Decisions filters what is sent. Empty means refusals and denials
	// only, which is the useful default: shipping every allowed query to a
	// third party is a firehose and a disclosure nobody asked for.
	Decisions []string
	// BufferSize bounds the queue. Zero uses a default.
	BufferSize int
	// Timeout bounds one delivery.
	Timeout time.Duration
}

// Defaults for a webhook.
const (
	DefaultWebhookBuffer  = 256
	DefaultWebhookTimeout = 10 * time.Second
)

// Webhook is an AuditSink that posts events to an endpoint.
type Webhook struct {
	url     string
	secret  []byte
	want    map[string]bool
	client  *http.Client
	log     *slog.Logger
	queue   chan Event
	done    chan struct{}
	closing sync.Once

	dropped atomic64
	sent    atomic64
	failed  atomic64
}

// atomic64 is a tiny counter, kept local so the struct stays readable.
type atomic64 struct {
	mu sync.Mutex
	n  uint64
}

func (a *atomic64) add() {
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
}

func (a *atomic64) load() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// NewWebhook builds a sink and starts its sender.
func NewWebhook(opts WebhookOptions) (*Webhook, error) {
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("the webhook URL could not be parsed: %w", err)
	}
	// http is refused off loopback. The payload says which identities are
	// asking what, and putting that on a plaintext connection to a third
	// party is a disclosure the operator probably did not intend.
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf(
			"a webhook to %s must be https: the payload names which identities "+
				"asked what, and on plain http that is readable by anything on the "+
				"path. Loopback is allowed for local development", u.Host)
	}

	want := map[string]bool{}
	for _, d := range opts.Decisions {
		want[strings.ToLower(strings.TrimSpace(d))] = true
	}
	if len(want) == 0 {
		// Refusals and denials. Shipping every allowed query to a third
		// party is a firehose, and the thing worth being told about is the
		// question that could not be answered.
		want["refused"] = true
		want["denied"] = true
	}

	size := opts.BufferSize
	if size <= 0 {
		size = DefaultWebhookBuffer
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}

	w := &Webhook{
		url:    opts.URL,
		secret: []byte(opts.Secret),
		want:   want,
		client: &http.Client{Timeout: timeout},
		queue:  make(chan Event, size),
		done:   make(chan struct{}),
	}
	go w.run()
	return w, nil
}

// WithLogging records delivery failures.
func (w *Webhook) WithLogging(lg *slog.Logger) *Webhook {
	w.log = lg
	return w
}

// Write queues an event. It never blocks.
//
// This is called on the query path, so it must return immediately whatever
// the receiver is doing. A full buffer drops and counts; the alternative,
// blocking until the webhook catches up, would make somebody else's slow
// endpoint into your slow warehouse.
func (w *Webhook) Write(e Event) {
	if !w.want[strings.ToLower(e.Decision)] {
		return
	}
	select {
	case w.queue <- e:
	default:
		w.dropped.add()
	}
}

// Stats reports delivery, for health.
func (w *Webhook) Stats() (sent, failed, dropped uint64) {
	return w.sent.load(), w.failed.load(), w.dropped.load()
}

// Close stops the sender and waits briefly for the queue to drain.
func (w *Webhook) Close() error {
	w.closing.Do(func() {
		close(w.queue)
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			// The process is stopping and a receiver that has gone away must
			// not hold the exit open.
		}
	})
	return nil
}

func (w *Webhook) run() {
	defer close(w.done)
	for e := range w.queue {
		w.deliver(e)
	}
}

func (w *Webhook) deliver(e Event) {
	body, err := json.Marshal(e)
	if err != nil {
		w.failed.add()
		return
	}

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		w.failed.add()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "truegrain")
	if len(w.secret) > 0 {
		mac := hmac.New(sha256.New, w.secret)
		mac.Write(body)
		req.Header.Set("X-Truegrain-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.failed.add()
		if w.log != nil {
			// The URL is logged and the payload is not. An operator needs to
			// know which endpoint is failing; the event is already in the
			// audit record and does not need repeating into the log.
			w.log.Warn("audit webhook delivery failed",
				"url", w.url, "error", err.Error())
		}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		w.failed.add()
		if w.log != nil {
			w.log.Warn("audit webhook rejected the event",
				"url", w.url, "status", resp.StatusCode)
		}
		return
	}
	w.sent.add()
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}
