package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/rk-chavali/truegrain/internal/govern"
)

// Caching a result, and the two things that make it safe.
//
// # Where it sits
//
// After the governance gate, never before it. A cache consulted before the
// gate would serve a result to a caller who was about to be denied, which
// is not a performance optimisation, it is an access control bypass with a
// latency benefit. By the time anything is looked up here, Compile has run:
// the caller's column access has been checked and their row policy has
// already been folded into the statement.
//
// # What the key is
//
// The compiled SQL, its bound parameters, the dialect, the model version
// and the caller's subject.
//
// The first three are obvious. The model version is what makes a reload
// correct: a model change produces a different digest, so nothing from the
// old definitions can be served under the new ones, and no invalidation
// pass is needed because the old entries simply stop matching.
//
// The subject is the one that needs justifying, because in principle it is
// redundant: row policy is applied before compilation, so two callers with
// different row access already produce different SQL and would not collide.
// It is in the key anyway. The cost is a lower hit rate between callers who
// genuinely share a policy; the benefit is that a bug in row-policy
// application cannot turn into one caller reading another's rows. On a path
// where the failure is a data leak, the cheap insurance is worth more than
// the hit rate.
//
// # What is not cached
//
// Refusals. They are cheap to recompute, they never reach the warehouse,
// and caching one would mean a caller whose access was just granted keeps
// being refused until it expires.
//
// Errors. A warehouse that failed once should be asked again; that is what
// the retry logic is for.

// Cache holds recent results.
//
// ponytail: a map with a TTL sweep, not an LRU. The entry count is bounded
// and a query result is a few kilobytes; the arithmetic that makes an LRU
// worth its bookkeeping does not apply to a few hundred entries. Revisit if
// somebody wants a gigabyte cache.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry
	order   []string // insertion order, for eviction when full

	ttl     time.Duration
	maxSize int
	now     func() time.Time

	hits, misses uint64
}

type entry struct {
	result  *Result
	expires time.Time
}

// CacheOptions configure it.
type CacheOptions struct {
	// TTL bounds how stale an answer may be. Required: a cache with no
	// expiry serves yesterday's number forever, and the whole product is
	// about a number being right.
	TTL time.Duration
	// MaxEntries bounds memory. Zero uses a default.
	MaxEntries int
}

// DefaultCacheEntries is the cap when none is given.
const DefaultCacheEntries = 512

// NewCache builds a cache.
func NewCache(opts CacheOptions) (*Cache, error) {
	if opts.TTL <= 0 {
		return nil, fmt.Errorf(
			"a result cache needs a TTL: without one it serves an answer from " +
				"an arbitrarily old warehouse state, and nothing would ever say so")
	}
	size := opts.MaxEntries
	if size <= 0 {
		size = DefaultCacheEntries
	}
	return &Cache{
		entries: map[string]*entry{},
		ttl:     opts.TTL,
		maxSize: size,
		now:     time.Now,
	}, nil
}

// TTL reports the configured freshness bound, for health.
func (c *Cache) TTL() time.Duration { return c.ttl }

// Stats reports hits and misses, for health and for deciding whether the
// cache is earning its keep.
func (c *Cache) Stats() (hits, misses uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// key builds the cache key.
//
// Hashed rather than concatenated, because the compiled SQL can be long and
// the parameters can contain a caller's filter values, which must not sit
// in a map key that might end up in a heap dump or a log line.
func cacheKey(subject, sql, dialect, modelVersion string, params []any) string {
	h := sha256.New()
	// Length-prefixed, so two different splits of the same bytes cannot
	// collide: a subject of "ab" with SQL "c" must not key the same as "a"
	// with "bc".
	for _, part := range []string{subject, sql, dialect, modelVersion} {
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	for _, p := range params {
		s := fmt.Sprint(p)
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// get returns a cached result, if one is live.
func (c *Cache) get(key string) (*Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok || c.now().After(e.expires) {
		c.misses++
		return nil, false
	}
	c.hits++
	return e.result, true
}

// put stores a result.
func (c *Cache) put(key string, res *Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sweepLocked()
	if _, exists := c.entries[key]; !exists {
		// Oldest out when full. A query whose result no longer fits is
		// served from the warehouse, which is correct and merely slower.
		for len(c.entries) >= c.maxSize && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = &entry{result: res, expires: c.now().Add(c.ttl)}
}

func (c *Cache) sweepLocked() {
	now := c.now()
	kept := c.order[:0]
	for _, k := range c.order {
		e, ok := c.entries[k]
		if !ok {
			continue
		}
		if now.After(e.expires) {
			delete(c.entries, k)
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
}

// cached looks a compiled query up.
//
// A separate method on Engine rather than inline, so the conditions under
// which a result may be served are in one readable place.
func (e *Engine) cached(id govern.Identity, c *Compiled) (*Result, string, bool) {
	if e.cache == nil {
		return nil, "", false
	}
	key := cacheKey(id.Subject, c.SQL, c.Dialect, c.ModelVersion, c.Params)
	res, ok := e.cache.get(key)
	return res, key, ok
}
