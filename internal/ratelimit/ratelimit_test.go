package ratelimit

import (
	"testing"
	"time"
)

// TestAllowEnforcesTheLimit is the basic contract.
func TestAllowEnforcesTheLimit(t *testing.T) {
	l := New(3, time.Hour)
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("request %d was refused inside the budget", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("request 4 was allowed past a limit of 3")
	}
	// A different key has its own budget: the limit is per source, not global.
	if !l.Allow("5.6.7.8") {
		t.Fatal("a different source was refused")
	}
}

// TestWindowRollover proves the window is a window and not a permanent counter.
func TestWindowRollover(t *testing.T) {
	l := New(1, time.Hour)
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now })

	if !l.Allow("k") {
		t.Fatal("first request refused")
	}
	if l.Allow("k") {
		t.Fatal("second request allowed inside the window")
	}
	now = now.Add(time.Hour)
	if !l.Allow("k") {
		t.Fatal("request refused after the window elapsed")
	}
}

// TestDisabledLimitAllowsEverything pins the direction of the zero value: a
// limiter that is off must never read as "reject everything". A misconfigured
// deployment then fails visibly as "no limiting" rather than as a dead endpoint.
func TestDisabledLimitAllowsEverything(t *testing.T) {
	for _, limit := range []int{0, -1} {
		l := New(limit, time.Hour)
		for i := 0; i < 50; i++ {
			if !l.Allow("k") {
				t.Fatalf("limit=%d refused a request", limit)
			}
		}
	}
	var nilLimiter *Limiter
	if !nilLimiter.Allow("k") {
		t.Fatal("a nil limiter refused a request")
	}
}

// TestSweepReclaimsBuckets guards the unbounded-growth failure: a flood from many
// distinct addresses must not grow the map forever.
func TestSweepReclaimsBuckets(t *testing.T) {
	l := New(5, time.Minute)
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now })

	for i := 0; i < 100; i++ {
		l.Allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if l.Size() == 0 {
		t.Fatal("no buckets tracked")
	}
	now = now.Add(2 * time.Minute)
	l.Allow("fresh")
	if got := l.Size(); got > 2 {
		t.Fatalf("after a sweep, %d buckets remain (want the stale ones gone)", got)
	}
}

func TestRetryAfterIsAtLeastOneSecond(t *testing.T) {
	l := New(1, time.Hour)
	if got := l.RetryAfter(); got != 3600 {
		t.Errorf("RetryAfter() = %d, want 3600", got)
	}
	var nilLimiter *Limiter
	if got := nilLimiter.RetryAfter(); got != 1 {
		t.Errorf("nil RetryAfter() = %d, want 1", got)
	}
}

// TestBlockedDoesNotConsume is the property the password throttle is built on:
// Blocked answers "is this key out of budget?" without spending any of it, so a
// caller can charge for failures only.
//
// If Blocked consumed, an honest reader that re-sends its password on every long
// poll would throttle itself out within a window — the exact failure it exists to
// prevent. If Blocked never reported true, guesses would be unlimited.
func TestBlockedDoesNotConsume(t *testing.T) {
	l := New(3, time.Hour)
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now })

	if l.Blocked("k") {
		t.Fatal("a fresh key reports blocked")
	}
	// Asking repeatedly must not use the budget up.
	for i := 0; i < 10; i++ {
		if l.Blocked("k") {
			t.Fatalf("Blocked consumed budget: blocked after %d queries", i)
		}
	}
	for i := 0; i < 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("Allow #%d refused inside the budget", i)
		}
	}
	if !l.Blocked("k") {
		t.Fatal("an exhausted key does not report blocked")
	}
	// Blocked must still not consume: if it did, the window would never end for a
	// key that keeps asking.
	if !l.Blocked("k") {
		t.Fatal("Blocked consumed budget on an exhausted key")
	}

	// A different key is unaffected, and the window resets on its own.
	if l.Blocked("other") {
		t.Error("Blocked leaked between keys")
	}
	now = now.Add(2 * time.Hour)
	if l.Blocked("k") {
		t.Error("the budget did not reset after the window")
	}
}

func TestBlockedOnANilOrDisabledLimiterIsFalse(t *testing.T) {
	// A zero limit means "unlimited", never "reject everything": the same
	// direction New documents for Allow.
	var nilLimiter *Limiter
	if nilLimiter.Blocked("k") {
		t.Error("a nil limiter reported blocked")
	}
	if New(0, time.Hour).Blocked("k") {
		t.Error("a disabled limiter reported blocked")
	}
}
