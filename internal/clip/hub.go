package clip

import (
	"sync"
	"time"
)

// hub wakes long-polling readers when a clip changes.
//
// Why long-polling instead of WebSocket (decided 2026-10-15): the dominant
// interaction is "A saves, B looks" — a single transition, not a stream. A
// WebSocket would add a second stateful protocol, heartbeats, reconnect logic
// and a connection-count limit, in exchange for latency nobody can perceive on
// a text clipboard. Long-polling reuses the same HTTP path, the same auth
// (none), and the same reverse-proxy rules, and it degrades on its own: if this
// hub refuses to wait, clients still get a correct answer, just later.
//
// The hub is deliberately in-process. Relay runs one process per service behind
// frp, so a cross-process bus would be machinery with no second participant.
type hub struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
	// max caps concurrent waiters so a burst of browsers cannot pin unbounded
	// memory or goroutines. Past the cap, Subscribe reports false and the
	// handler answers immediately — a correct, slower answer.
	max int
}

func newHub(max int) *hub {
	if max <= 0 {
		max = 1024
	}
	return &hub{waiters: make(map[string][]chan struct{}), max: max}
}

// subscribe registers interest in a suffix and returns the channel to wait on
// plus the function that must be called to release it.
//
// The returned channel is closed (never sent on) when the suffix changes, so a
// waiter cannot miss an update by arriving after the change but before the send:
// Publish closes the channels it saw, and a subscriber that registered later
// re-reads the clip first.
func (h *hub) subscribe(suffix string) (<-chan struct{}, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	total := 0
	for _, chs := range h.waiters {
		total += len(chs)
	}
	if total >= h.max {
		return nil, func() {}, false
	}

	ch := make(chan struct{})
	h.waiters[suffix] = append(h.waiters[suffix], ch)

	var once sync.Once
	release := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			list := h.waiters[suffix]
			for i, c := range list {
				if c == ch {
					list = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(list) == 0 {
				delete(h.waiters, suffix)
			} else {
				h.waiters[suffix] = list
			}
		})
	}
	return ch, release, true
}

// publish wakes every waiter registered for a suffix.
//
// It is called after the write is committed, never before: waking a reader that
// then reads stale data would make the client re-register and wait again, which
// is not incorrect but is needless churn.
func (h *hub) publish(suffix string) {
	h.mu.Lock()
	list := h.waiters[suffix]
	delete(h.waiters, suffix)
	h.mu.Unlock()

	for _, ch := range list {
		close(ch)
	}
}

// waiterCount is used by tests to prove that waiters are released on every exit
// path — including the timeout path, which is the one that leaks when a hub is
// written carelessly.
func (h *hub) waiterCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, chs := range h.waiters {
		total += len(chs)
	}
	return total
}

// longPollWindow caps how long one request may be held. Reverse proxies close
// idle connections; 25s stays well inside the usual 60s and keeps a client that
// has gone away from holding a goroutine for long.
const longPollWindow = 25 * time.Second
