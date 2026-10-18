package ratelimit

import "testing"

// TestConcurrencyCapsPerKey is the core contract: one key cannot take more than
// its share of open slots, and a different key is unaffected.
func TestConcurrencyCapsPerKey(t *testing.T) {
	c := NewConcurrency(2)
	if !c.Acquire("a") || !c.Acquire("a") {
		t.Fatal("the first two slots for one key were refused")
	}
	if c.Acquire("a") {
		t.Fatal("a third slot was granted past a cap of 2")
	}
	if !c.Acquire("b") {
		t.Fatal("a different key was refused; the cap is not per key")
	}

	// Releasing one slot makes exactly one more available.
	c.Release("a")
	if !c.Acquire("a") {
		t.Fatal("a released slot was not reusable")
	}
	if c.Acquire("a") {
		t.Fatal("the cap was exceeded after a release")
	}
}

// TestConcurrencyReleaseIsIdempotentAtZero: releasing more than was acquired
// must not make the count negative, which would let a caller hold more slots
// than the cap on the next round.
func TestConcurrencyReleaseIsIdempotentAtZero(t *testing.T) {
	c := NewConcurrency(1)
	c.Release("a")
	c.Release("a")
	if got := c.Count("a"); got != 0 {
		t.Fatalf("count after over-release = %d, want 0", got)
	}
	if !c.Acquire("a") {
		t.Fatal("the slot was not available after an over-release")
	}
	if c.Acquire("a") {
		t.Fatal("over-release widened the cap")
	}
}

// TestConcurrencyForgetsIdleKeys keeps the map bounded: keys are source
// addresses, so a map that keeps a zero entry per visitor grows forever.
func TestConcurrencyForgetsIdleKeys(t *testing.T) {
	c := NewConcurrency(4)
	for i := 0; i < 100; i++ {
		key := string(rune('a' + i%26))
		if !c.Acquire(key) {
			t.Fatalf("acquire failed for %q", key)
		}
		c.Release(key)
	}
	if got := c.Size(); got != 0 {
		t.Fatalf("%d idle keys are still tracked; the map is not bounded", got)
	}
}

// TestConcurrencyDisabledAllowsEverything pins the zero-value direction, the
// same as the limiter: an unset cap must read as "unlimited", never as "refuse".
func TestConcurrencyDisabledAllowsEverything(t *testing.T) {
	for _, limit := range []int{0, -1} {
		c := NewConcurrency(limit)
		for i := 0; i < 100; i++ {
			if !c.Acquire("k") {
				t.Fatalf("limit=%d refused a slot", limit)
			}
		}
	}
	var nilCap *Concurrency
	if !nilCap.Acquire("k") {
		t.Fatal("a nil concurrency cap refused a slot")
	}
}
