package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

// TestBansTriggerOnlyAfterTheThreshold is the core contract: a key is not
// blocked while it is merely accumulating strikes, and the refusal that reaches
// the threshold is the one that bans.
func TestBansTriggerOnlyAfterTheThreshold(t *testing.T) {
	b := NewBans(BanPolicy{After: 3, Window: 10 * time.Minute, Base: 15 * time.Minute, Max: time.Hour})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	for i := 1; i <= 2; i++ {
		banned, _, strikes := b.Strike("1.2.3.4")
		if banned {
			t.Fatalf("strike %d banned a key below the threshold", i)
		}
		if strikes != i {
			t.Fatalf("strike %d reported %d strikes", i, strikes)
		}
		if blocked, _ := b.Blocked("1.2.3.4"); blocked {
			t.Fatalf("strike %d blocked the key below the threshold", i)
		}
	}

	banned, d, strikes := b.Strike("1.2.3.4")
	if !banned {
		t.Fatal("the strike that reached the threshold did not ban")
	}
	if strikes != 3 {
		t.Fatalf("the banning strike reported %d strikes, want 3", strikes)
	}
	if d != 15*time.Minute {
		t.Fatalf("first ban = %s, want 15m", d)
	}
	blocked, remaining := b.Blocked("1.2.3.4")
	if !blocked {
		t.Fatal("a banned key was not reported blocked")
	}
	if remaining <= 0 || remaining > 15*time.Minute {
		t.Fatalf("remaining ban = %s, want (0, 15m]", remaining)
	}

	// A different key is untouched: bans are per key, not global.
	if blocked, _ := b.Blocked("5.6.7.8"); blocked {
		t.Fatal("a ban leaked onto another key")
	}
}

// TestBansExpire proves a ban is temporary. A ban list that never forgets is a
// way to permanently lock out an address, which for a shared proxy is a way to
// permanently lock out everybody behind it.
func TestBansExpire(t *testing.T) {
	b := NewBans(BanPolicy{After: 1, Window: time.Minute, Base: 15 * time.Minute, Max: time.Hour})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	if banned, _, _ := b.Strike("k"); !banned {
		t.Fatal("a threshold of 1 did not ban on the first strike")
	}
	now = now.Add(14 * time.Minute)
	if blocked, _ := b.Blocked("k"); !blocked {
		t.Fatal("the key was unblocked before the ban elapsed")
	}
	now = now.Add(2 * time.Minute)
	if blocked, _ := b.Blocked("k"); blocked {
		t.Fatal("the key stayed blocked after the ban elapsed")
	}
}

// TestBansEscalateAndCap pins the escalation curve and, more importantly, that
// it cannot overflow into a negative duration — a negative ban reads as "not
// banned", which would silently turn the defence off for the worst offender.
//
// The offender keeps coming back immediately after each ban expires; that is the
// behaviour escalation is for, and it is also what keeps the entry alive through
// the sweep (see retain).
func TestBansEscalateAndCap(t *testing.T) {
	b := NewBans(BanPolicy{After: 1, Window: time.Minute, Base: time.Minute, Max: 8 * time.Minute})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	want := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute, // capped
		8 * time.Minute, // still capped
		8 * time.Minute,
	}
	for i, w := range want {
		banned, d, _ := b.Strike("k")
		if !banned {
			t.Fatalf("ban %d did not trigger", i+1)
		}
		if d != w {
			t.Fatalf("ban %d = %s, want %s", i+1, d, w)
		}
		if d <= 0 {
			t.Fatalf("ban %d is non-positive (%s), which reads as \"not banned\"", i+1, d)
		}
		// The offender waits out this ban and immediately returns.
		now = now.Add(d + time.Second)
	}
}

// TestBansForgetStrikesAcrossWindows proves the ban is about sustained abuse: a
// caller that trips the limiter once an hour forever is never banned.
func TestBansForgetStrikesAcrossWindows(t *testing.T) {
	b := NewBans(BanPolicy{After: 3, Window: 10 * time.Minute, Base: time.Minute, Max: time.Hour})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	for i := 0; i < 20; i++ {
		if banned, _, _ := b.Strike("k"); banned {
			t.Fatalf("iteration %d was banned; strikes from earlier windows were not forgotten", i)
		}
		now = now.Add(11 * time.Minute)
	}
}

// TestBansDisabledNeverBlocks pins the direction of the zero value, the same way
// the limiter's own test does: an unconfigured ban list must never reject.
func TestBansDisabledNeverBlocks(t *testing.T) {
	for _, after := range []int{0, -1} {
		b := NewBans(BanPolicy{After: after})
		for i := 0; i < 100; i++ {
			if banned, _, _ := b.Strike("k"); banned {
				t.Fatalf("After=%d banned a key", after)
			}
		}
		if blocked, _ := b.Blocked("k"); blocked {
			t.Fatalf("After=%d reported a block", after)
		}
	}
	var nilBans *Bans
	if blocked, _ := nilBans.Blocked("k"); blocked {
		t.Fatal("a nil ban list reported a block")
	}
	if banned, _, _ := nilBans.Strike("k"); banned {
		t.Fatal("a nil ban list banned a key")
	}
}

// TestBansSweepReclaimsMemory is the guard against a scanner that varies its
// source address: the map has to shrink again, or the ban list becomes the
// memory-exhaustion vector it was added to prevent.
func TestBansSweepReclaimsMemory(t *testing.T) {
	b := NewBans(BanPolicy{After: 5, Window: time.Minute, Base: time.Minute, Max: time.Minute})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	for i := 0; i < 1000; i++ {
		b.Strike(fmt.Sprintf("203.0.113.%d", i))
	}
	if b.Size() == 0 {
		t.Fatal("no entries were tracked, so the sweep assertion proves nothing")
	}
	// Long enough past retain (Max+Window = 2m) with no activity: none of these
	// keys reached the ban threshold, so nothing is holding an entry alive.
	now = now.Add(5 * time.Minute)
	b.Strike("fresh")
	if got := b.Size(); got > 2 {
		t.Fatalf("sweep left %d entries; stale keys are not being reclaimed", got)
	}
}

// TestBansRememberEscalationAcrossABan is the counterpart of the sweep test: a
// repeat offender who waits out a ban and comes straight back must meet a longer
// ban, not a reset. Getting this wrong makes escalation cosmetic.
func TestBansRememberEscalationAcrossABan(t *testing.T) {
	b := NewBans(BanPolicy{After: 1, Window: 10 * time.Minute, Base: 15 * time.Minute, Max: time.Hour})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	_, first, _ := b.Strike("k")
	// Sit out the first ban, then return immediately.
	now = now.Add(first + time.Second)
	_, second, _ := b.Strike("k")
	if second <= first {
		t.Fatalf("second ban = %s, first = %s; escalation was forgotten", second, first)
	}
}

// TestBansFailOpenAtTheEntryCeiling: the ban list must not become its own
// memory-exhaustion vector. At the ceiling it stops tracking NEW keys rather
// than growing without bound, and "not tracked" must mean "not banned".
func TestBansFailOpenAtTheEntryCeiling(t *testing.T) {
	b := NewBans(BanPolicy{After: 1, Window: time.Minute, Base: time.Minute, Max: time.Minute})
	b.maxEntries = 3
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if banned, _, _ := b.Strike(fmt.Sprintf("k%d", i)); !banned {
			t.Fatalf("key %d was not tracked before the ceiling", i)
		}
	}
	if banned, _, _ := b.Strike("overflow"); banned {
		t.Fatal("a key past the entry ceiling was tracked")
	}
	if got := b.Size(); got > 3 {
		t.Fatalf("the map grew to %d entries past its ceiling of 3", got)
	}
}

// TestBansKeepBannedKeysThroughTheSweep: an entry that is still banned must
// survive a sweep, or the ban would be silently lifted by unrelated traffic.
func TestBansKeepBannedKeysThroughTheSweep(t *testing.T) {
	b := NewBans(BanPolicy{After: 1, Window: time.Minute, Base: time.Hour, Max: time.Hour})
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })

	b.Strike("attacker")
	// A long ban (1h) outlives the 1m window, so a sweep at +2m must not drop it.
	now = now.Add(2 * time.Minute)
	b.Strike("someone-else")
	if blocked, _ := b.Blocked("attacker"); !blocked {
		t.Fatal("a sweep lifted an active ban")
	}
}
