// Package ratelimit provides the small in-process throttle both Relay services
// need.
//
// Scope: this is a speed bump for casual abuse, not a defence. It keys on the
// client address, and an attacker with a handful of real addresses — or a single
// IPv6 /64 — gets a fresh bucket per source. The real backstop for a public write
// endpoint is the global quota in the store, which is IP-independent. The two are
// deliberately separate so that "the limiter is bypassed" degrades from "unlimited
// abuse" to "abuse, but bounded".
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter per key.
//
// A fixed window (rather than a sliding one) is chosen because the cost of the
// approximation is bounded and known: a caller can get at most 2× the limit
// across a window boundary, and windows are an hour long. In exchange the state
// per key is two integers instead of a slice of timestamps, which matters when
// the key space is "every address on the internet".
type Limiter struct {
	limit  int
	window time.Duration
	// now is injectable so tests can advance time instead of sleeping. A
	// sleep-based limiter test both slows the suite and stops testing the
	// boundary condition it claims to cover.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]bucket
	// lastSweep bounds the map: without it, a flood from many addresses grows
	// memory without limit.
	lastSweep time.Time
}

type bucket struct {
	start time.Time
	count int
}

// New builds a limiter allowing limit requests per window per key.
//
// A non-positive limit disables limiting entirely. That direction matters: a
// zero value must never be read as "reject everything".
func New(limit int, window time.Duration) *Limiter {
	if window <= 0 {
		window = time.Hour
	}
	return &Limiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		buckets: make(map[string]bucket),
	}
}

// Count records one attempt against key's budget and reports whether it was
// within budget.
func (l *Limiter) Allow(key string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		l.buckets[key] = bucket{start: now, count: 1}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	l.buckets[key] = b
	return true
}

// Blocked reports whether key has already used up its budget, WITHOUT consuming
// any of it.
//
// It exists so a caller can charge for failures only. A password check must be
// throttled, but a *correct* password must not consume anything — otherwise a
// legitimate reader that re-sends its password on every long poll (which is what
// the clip page does) would eventually throttle itself out. Callers pair it with
// Allow at the failure site: Blocked to decide whether to do the expensive work
// at all, Allow to charge the attempt that failed.
//
// The pair is deliberately not atomic. A race lets a few extra attempts through
// under concurrency; that is the right trade for a speed bump — the alternative
// is making every honest reader pay for the guesser's budget, which fails
// closed on the wrong party.
func (l *Limiter) Blocked(key string) bool {
	if l == nil || l.limit <= 0 {
		return false
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		return false
	}
	return b.count >= l.limit
}

// RetryAfter is an upper bound on how long the caller should wait: the whole
// window, rather than the exact remainder, which would leak the caller's own
// history back to them.
func (l *Limiter) RetryAfter() int {
	if l == nil || l.window <= 0 {
		return 1
	}
	secs := int((l.window + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

// sweepLocked drops expired buckets, at most once per window. Amortised O(1) per
// call in the common case, because a flood of distinct addresses is exactly the
// case where the map would otherwise grow forever.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	for key, b := range l.buckets {
		if now.Sub(b.start) >= l.window {
			delete(l.buckets, key)
		}
	}
	l.lastSweep = now
}

// SetClock replaces the clock. Test-only; it must not be called concurrently
// with Allow.
func (l *Limiter) SetClock(fn func() time.Time) { l.now = fn }

// Size reports how many buckets are tracked, so a test can assert that the sweep
// actually reclaims memory.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
