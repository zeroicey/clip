package ratelimit

import (
	"sync"
	"time"
)

// Bans escalates repeated refusals into a temporary block.
//
// It exists because a fixed-window throttle is a speed bump that never ends:
// an address that ignores Retry-After and keeps asking forever is refused each
// time, but it is never told to go away, and every one of those refusals still
// costs a lookup and a log line. A ban is the escalation step — once a caller
// has been refused enough times inside a window, the answer becomes "do not come
// back for a while" and the work stops happening.
//
// What it is not: a defence. The key is a source address, so an attacker with
// many addresses (or a single IPv6 /64) has as many bans as addresses. This is
// the same honest limit the package documents for the throttles; the global
// quota in the store remains the IP-independent backstop.
//
// The failure direction matters more than the mechanism. Every entry expires,
// nothing is persisted, and a zero-value policy disables banning entirely rather
// than blocking everyone. A ban list that can only grow, or that survives a
// restart, is a way to lock the operator out of their own service with no way
// back short of editing a file.
type Bans struct {
	policy BanPolicy
	// retain is how long an entry is kept after its last activity. It must
	// outlive the longest ban, or an offender who waits out a ban would come back
	// as a first-timer and the escalation would never escalate.
	retain time.Duration
	// maxEntries is a hard ceiling on tracked keys. At the ceiling a NEW key is
	// simply not tracked, which fails open: the alternative is a flood of distinct
	// addresses turning the ban list itself into the memory-exhaustion vector.
	maxEntries int
	// now is injectable so a test can advance time instead of sleeping through a
	// fifteen-minute ban.
	now func() time.Time

	mu        sync.Mutex
	entries   map[string]*banEntry
	lastSweep time.Time
}

// BanPolicy describes when and for how long a key is banned.
//
// After <= 0 disables the whole thing. Window, Base and Max all fall back to
// sane values when left zero, so a half-filled policy is a working policy rather
// than an accidental one.
type BanPolicy struct {
	// After is how many refusals inside Window trigger a ban.
	After int
	// Window is how long refusals accumulate before they are forgotten.
	Window time.Duration
	// Base is the first ban's duration; every subsequent ban doubles it.
	Base time.Duration
	// Max caps the escalation, so a repeat offender cannot be banned forever.
	Max time.Duration
}

type banEntry struct {
	// strikes counts refusals inside the current window.
	strikes int
	// windowStart is when the current strike window opened.
	windowStart time.Time
	// bannedUntil is the end of the current ban, zero when not banned.
	bannedUntil time.Time
	// banCount is how many times this key has been banned, which is what makes
	// the durations escalate.
	banCount int
	// lastSeen keeps an entry alive while a caller is active, so the sweep does
	// not drop an escalating offender between requests.
	lastSeen time.Time
}

// NewBans builds a ban list from a policy. A non-positive After returns a Bans
// that never bans, which is the only safe reading of "not configured".
func NewBans(policy BanPolicy) *Bans {
	if policy.Window <= 0 {
		policy.Window = 10 * time.Minute
	}
	if policy.Base <= 0 {
		policy.Base = 15 * time.Minute
	}
	if policy.Max <= 0 {
		policy.Max = 24 * time.Hour
	}
	if policy.Max < policy.Base {
		policy.Max = policy.Base
	}
	return &Bans{
		policy:     policy,
		retain:     policy.Max + policy.Window,
		maxEntries: 100_000,
		now:        time.Now,
		entries:    make(map[string]*banEntry),
	}
}

// Blocked reports whether key is banned and how much of the ban is left.
//
// It does not consume anything, so it can be asked on every request. A key that
// is merely accumulating strikes is not blocked.
func (b *Bans) Blocked(key string) (bool, time.Duration) {
	if b == nil || b.policy.After <= 0 {
		return false, 0
	}
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepLocked(now)

	e, ok := b.entries[key]
	if !ok {
		return false, 0
	}
	if now.Before(e.bannedUntil) {
		e.lastSeen = now
		return true, e.bannedUntil.Sub(now)
	}
	return false, 0
}

// Strike records one refusal against key and reports whether that refusal
// triggered a ban, for how long, and the key's strike count after this one.
//
// The caller charges a strike only when it has already refused the request (a
// 429), never for an honest mistake: a person who mistypes a password once must
// not be three refusals away from being locked out of the service.
func (b *Bans) Strike(key string) (banned bool, duration time.Duration, strikes int) {
	var d time.Duration
	if b == nil || b.policy.After <= 0 {
		return false, 0, 0
	}
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepLocked(now)

	e, ok := b.entries[key]
	if !ok {
		if len(b.entries) >= b.maxEntries {
			// At the ceiling. Refusing to track fails OPEN, which is the right
			// direction for a defence that must never take the service down by
			// itself.
			return false, 0, 0
		}
		e = &banEntry{windowStart: now}
		b.entries[key] = e
	}
	// A new window forgets the previous strikes. This is what makes the ban about
	// sustained abuse rather than about a lifetime total.
	if now.Sub(e.windowStart) >= b.policy.Window {
		e.strikes = 0
		e.windowStart = now
	}
	e.strikes++
	e.lastSeen = now

	if e.strikes < b.policy.After {
		return false, 0, e.strikes
	}

	// Ban. The strike counter resets with the ban so that the *next* window of
	// refusals is what extends the ban, not the same burst counted twice.
	triggered := e.strikes
	e.strikes = 0
	e.windowStart = now
	e.banCount++
	d = b.escalated(e.banCount)
	e.bannedUntil = now.Add(d)
	return true, d, triggered
}

// escalated returns the ban duration for the n-th ban: Base doubled n-1 times,
// capped at Max. The loop is used instead of a shift because the cap has to be
// checked at each step — Base<<63 overflows into a negative duration, and a
// negative ban is "not banned", which is the worst possible failure here.
func (b *Bans) escalated(n int) time.Duration {
	d := b.policy.Base
	for i := 1; i < n; i++ {
		if d >= b.policy.Max {
			return b.policy.Max
		}
		d *= 2
	}
	if d > b.policy.Max {
		return b.policy.Max
	}
	return d
}

// sweepLocked drops entries that are neither banned nor recently active, at most
// once per window. Without it a scanner that varies its source address grows the
// map without bound — the same failure the limiter's own sweep exists to stop.
//
// "Recently active" is deliberately longer than the longest ban (see retain):
// an offender that keeps knocking during its ban keeps its entry, and therefore
// its escalation. Only sustained silence forgets.
func (b *Bans) sweepLocked(now time.Time) {
	if now.Sub(b.lastSweep) < b.policy.Window {
		return
	}
	for key, e := range b.entries {
		if now.Before(e.bannedUntil) {
			continue
		}
		if now.Sub(e.lastSeen) < b.retain {
			continue
		}
		delete(b.entries, key)
	}
	b.lastSweep = now
}

// Size reports how many keys are tracked, so a test can prove the sweep
// reclaims memory.
func (b *Bans) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// SetClock replaces the clock. Test-only; it must not be called concurrently
// with Blocked or Strike.
func (b *Bans) SetClock(fn func() time.Time) { b.now = fn }
