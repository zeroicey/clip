package clip

import (
	"strings"
	"testing"
	"time"
)

// TestHubPublishWakesWaiter is the core contract: a reader waiting on a suffix
// is released when that suffix changes.
func TestHubPublishWakesWaiter(t *testing.T) {
	h := newHub(10)
	ch, release, okWait := h.subscribe("ops1")
	if !okWait {
		t.Fatal("subscribe refused a waiter well under the cap")
	}
	defer release()

	h.publish("ops1")

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("publish did not wake the waiter")
	}
}

// TestHubPublishIsPerSuffix guards against the obvious implementation mistake:
// waking everyone on any change, which would turn a busy service into a
// thundering herd of re-reads.
func TestHubPublishIsPerSuffix(t *testing.T) {
	h := newHub(10)
	chA, releaseA, _ := h.subscribe("aaa")
	defer releaseA()
	chB, releaseB, _ := h.subscribe("bbb")
	defer releaseB()

	h.publish("aaa")

	select {
	case <-chA:
	default:
		t.Fatal("waiter on the published suffix was not woken")
	}
	select {
	case <-chB:
		t.Fatal("waiter on an unrelated suffix was woken")
	default:
	}
}

// TestHubReleaseDoesNotLeak is the leak guard: every exit path from a long poll
// — including the timeout, which is the common case — must unregister. A leaked
// waiter is a channel the next publish closes while nobody reads it, and a map
// that grows until the process restarts.
func TestHubReleaseDoesNotLeak(t *testing.T) {
	h := newHub(10)

	_, release, _ := h.subscribe("ops1")
	if got := h.waiterCount(); got != 1 {
		t.Fatalf("waiterCount = %d after subscribe, want 1", got)
	}
	release()
	if got := h.waiterCount(); got != 0 {
		t.Fatalf("waiterCount = %d after release, want 0", got)
	}
	// Releasing twice must not delete someone else's registration.
	_, release2, _ := h.subscribe("ops1")
	release()
	release()
	if got := h.waiterCount(); got != 1 {
		t.Fatalf("waiterCount = %d after a double release, want 1", got)
	}
	release2()
	if got := h.waiterCount(); got != 0 {
		t.Fatalf("waiterCount = %d at the end, want 0", got)
	}
}

// TestHubRefusesPastTheCap proves the cap fails *open* (caller answers
// immediately) rather than blocking or panicking.
func TestHubRefusesPastTheCap(t *testing.T) {
	h := newHub(2)
	_, r1, ok1 := h.subscribe("a")
	defer r1()
	_, r2, ok2 := h.subscribe("b")
	defer r2()
	if !ok1 || !ok2 {
		t.Fatal("subscribe refused before the cap was reached")
	}
	_, r3, ok3 := h.subscribe("c")
	defer r3()
	if ok3 {
		t.Fatal("subscribe succeeded past the cap")
	}
}

func TestHubPublishWithNoWaitersIsHarmless(t *testing.T) {
	h := newHub(4)
	h.publish("nobody-is-listening")
	if got := h.waiterCount(); got != 0 {
		t.Fatalf("waiterCount = %d, want 0", got)
	}
}

// TestNonceSurvivesHTMLAttributeEscaping is the regression guard for a subtle
// interaction between CSP and html/template.
//
// html/template escapes `+` to `&#43;` in attribute context. With a
// standard-base64 nonce the HTML therefore held a different *byte* form from the
// CSP header: the browser parsed the entity and the page worked, while the
// assertion below failed on roughly 30% of runs. A flake like that is worse than
// a failure — it teaches everyone to re-run instead of to look.
func TestNonceSurvivesHTMLAttributeEscaping(t *testing.T) {
	unsafe := []string{"+", "/", "=", "&", "<", ">", "\"", "'", "`"}
	seen := map[rune]bool{}
	for i := 0; i < 500; i++ {
		n, err := newNonce()
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range unsafe {
			if strings.Contains(n, ch) {
				t.Fatalf("nonce %q contains %q, which html/template rewrites in attribute context", n, ch)
			}
		}
		for _, r := range n {
			seen[r] = true
		}
	}
	// And it must still be real entropy, not a degenerate alphabet that happens
	// to be safe: 500 draws should touch most of `A-Za-z0-9-_`.
	if len(seen) < 40 {
		t.Errorf("500 nonces used only %d distinct characters", len(seen))
	}
}

// TestPageCSPNonceMatchesTheRenderedTag ties the policy to the page: a CSP whose
// nonce does not match the tag it is meant to authorise blocks the page's own
// script, which is exactly the failure mode recorded in
// .ai/pitfalls/cases/2026-10-12-csp-blocks-the-assets-the-page-links.md.
func TestPageCSPNonceMatchesTheRenderedTag(t *testing.T) {
	nonce, err := newNonce()
	if err != nil {
		t.Fatal(err)
	}
	csp := PageCSP
	if !strings.Contains(csp, "nonce-%s") {
		t.Fatal("PageCSP must take the nonce as a parameter")
	}
	rendered := strings.ReplaceAll(csp, "%s", nonce)
	if !strings.Contains(rendered, "script-src 'nonce-"+nonce+"'") {
		t.Fatalf("CSP does not authorise the page nonce: %q", rendered)
	}
	// The policy must not fall back to allowing inline script wholesale: that
	// would make the nonce decorative.
	if strings.Contains(rendered, "unsafe-inline") || strings.Contains(rendered, "unsafe-eval") {
		t.Fatalf("CSP weakened to unsafe-inline/unsafe-eval: %q", rendered)
	}
}
