package ratelimit

import "sync"

// Concurrency caps how many requests one key may have in flight at once.
//
// It is a different shape from Limiter on purpose. Limiter bounds a *rate* (how
// many requests per window); this bounds a *count* (how many are open right
// now). The two are not interchangeable for a long poll: a reader parked for 25
// seconds costs one request of budget and one open connection, so a rate limit
// alone lets a single address hold every long-poll slot the service has while
// staying comfortably inside its per-minute allowance.
//
// Past the cap, Acquire reports false. The caller decides what that means; for a
// long poll the right answer is to reply immediately instead of parking, which
// is slower but still correct.
type Concurrency struct {
	limit int

	mu     sync.Mutex
	counts map[string]int
}

// NewConcurrency builds a per-key concurrency cap. A non-positive limit means
// unlimited (Acquire always succeeds), matching the rest of the package: an
// unset knob must never read as "reject everything".
func NewConcurrency(limit int) *Concurrency {
	return &Concurrency{limit: limit, counts: make(map[string]int)}
}

// Acquire reserves one slot for key and reports whether there was room.
//
// A caller that gets true must call Release exactly once, on every exit path.
func (c *Concurrency) Acquire(key string) bool {
	if c == nil || c.limit <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[key] >= c.limit {
		return false
	}
	c.counts[key]++
	return true
}

// Release returns a slot reserved by Acquire. Releasing a key that holds nothing
// is a no-op rather than a negative count, so a double release cannot widen the
// cap.
func (c *Concurrency) Release(key string) {
	if c == nil || c.limit <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[key] <= 1 {
		// Deleting rather than storing zero is what keeps the map bounded: keys
		// are source addresses, and the map must not grow with every visitor.
		delete(c.counts, key)
		return
	}
	c.counts[key]--
}

// Count reports how many slots key currently holds. Test-only.
func (c *Concurrency) Count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

// Size reports how many distinct keys are tracked. Test-only.
func (c *Concurrency) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.counts)
}
