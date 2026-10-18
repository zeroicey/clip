package clip

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file covers the request-level defence layer in middleware.go. The unit
// behaviour of the ban list and the concurrency cap lives in
// internal/ratelimit; what is asserted here is the wiring: which requests are
// charged, what a refusal looks like on each namespace, and — most importantly —
// who must never be banned.

// TestSecurityHeadersAreOnEveryResponseShape is the point of doing this in a
// middleware rather than per handler: the error paths are where a security
// header is most often forgotten, so they are asserted first-class.
func TestSecurityHeadersAreOnEveryResponseShape(t *testing.T) {
	r := testServer(t)
	if w := do(t, r, "PUT", "/api/clips/ops1", map[string]any{"body": "hello"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("setup: create = %d (%s)", w.Code, w.Body.String())
	}

	paths := []struct {
		method, path string
	}{
		{"GET", "/"},               // landing page
		{"GET", "/ops1"},           // clipboard page
		{"GET", "/brandnew"},       // empty editor (200)
		{"GET", "/ops1/raw"},       // raw text
		{"GET", "/api/clips/ops1"}, // JSON API
		{"GET", "/api/nope"},       // unknown API path (envelope 404)
		{"POST", "/ops1"},          // 405 from the public namespace
		{"GET", "/a/b/c"},          // text/plain 404
		{"GET", "/favicon.ico"},    // 204
	}

	want := map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for _, p := range paths {
		w := do(t, r, p.method, p.path, nil, nil)
		for name, value := range want {
			if got := w.Header().Get(name); got != value {
				t.Errorf("%s %s: %s = %q, want %q", p.method, p.path, name, got, value)
			}
		}
		// Contains, not equality: the page handlers install a stricter
		// "noindex, nofollow, noarchive", and stricter is the point.
		if got := w.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("%s %s: X-Robots-Tag = %q, want it to carry noindex", p.method, p.path, got)
		}
		if w.Header().Get("Permissions-Policy") == "" {
			t.Errorf("%s %s: no Permissions-Policy", p.method, p.path)
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s %s: no Content-Security-Policy", p.method, p.path)
		}
	}

	// The baseline policy is what the non-page answers keep. The HTML pages must
	// replace it with their nonce policy — a page that silently kept the baseline
	// would be a page whose script never runs.
	for _, path := range []string{"/api/nope", "/a/b/c"} {
		csp := do(t, r, "GET", path, nil, nil).Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "nonce-") {
			t.Errorf("%s kept the wrong CSP: %q", path, csp)
		}
	}
	for _, path := range []string{"/", "/ops1"} {
		csp := do(t, r, "GET", path, nil, nil).Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "nonce-") {
			t.Errorf("%s did not install its nonce policy: %q", path, csp)
		}
	}
}

// TestNoHSTSFromTheApplication pins that the app leaves HSTS to the TLS layer.
//
// The reverse proxy in front of this service already sends it, with a longer
// max-age and `preload`; a second, weaker header from the app showed up on every
// live response during the 2026-10-18 deploy. This test is the guard against
// reintroducing it.
func TestNoHSTSFromTheApplication(t *testing.T) {
	for _, base := range []string{"http://test.local", "https://clip.example"} {
		r := testServer(t, func(c *Config) { c.PublicBaseURL = base })
		if got := do(t, r, "GET", "/api/health", nil, nil).Header().Get("Strict-Transport-Security"); got != "" {
			t.Errorf("origin %s: the application sent HSTS (%q); the reverse proxy owns it", base, got)
		}
	}
}

// TestRequestBudgetCoversReads is the gap this layer closes: before it, only
// writes were throttled, so a single address could drive the read paths (and the
// database behind them) without limit.
func TestRequestBudgetCoversReads(t *testing.T) {
	r := testServer(t, func(c *Config) { c.RequestPerMinute = 2; c.BanAfter = 0 })

	for i := 0; i < 2; i++ {
		if w := do(t, r, "GET", "/api/clips/ops1", nil, nil); w.Code != http.StatusOK {
			t.Fatalf("read %d inside the budget = %d", i+1, w.Code)
		}
	}
	w := do(t, r, "GET", "/api/clips/ops1", nil, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("read past the budget = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 429 from the request budget carried no Retry-After")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("an /api refusal came back as %q, not the envelope", ct)
	}
}

// TestRequestBudgetIsPerSource: the budget must key on the source address, not
// be one global counter. A global counter is the failure mode where one busy
// client silences everyone.
func TestRequestBudgetIsPerSource(t *testing.T) {
	r := testServer(t, func(c *Config) { c.RequestPerMinute = 1; c.BanAfter = 0 })

	if w := doFrom(t, r, "198.51.100.7", "GET", "/api/clips/ops1", ""); w.Code != http.StatusOK {
		t.Fatalf("peer A first read = %d", w.Code)
	}
	if w := doFrom(t, r, "198.51.100.7", "GET", "/api/clips/ops1", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("peer A second read = %d, want 429", w.Code)
	}
	if w := doFrom(t, r, "203.0.113.9", "GET", "/api/clips/ops1", ""); w.Code != http.StatusOK {
		t.Errorf("a fresh peer = %d, want 200; the budget is not per source", w.Code)
	}
}

// TestRepeatedRefusalsBanTheSource walks the escalation end to end: a spent
// budget, then the ban, then the refusal shape on each namespace.
func TestRepeatedRefusalsBanTheSource(t *testing.T) {
	r := testServer(t, func(c *Config) {
		c.RequestPerMinute = 1
		c.BanAfter = 2
		c.BanFor = 15 * time.Minute
		c.BanMaxFor = time.Hour
	})

	// 1: inside budget. 2 and 3: refused, and the second refusal is the ban.
	if w := do(t, r, "GET", "/ops1", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("first request = %d", w.Code)
	}
	if w := do(t, r, "GET", "/ops1", nil, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", w.Code)
	}
	if w := do(t, r, "GET", "/ops1", nil, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("third request = %d, want 429 (the refusal that bans)", w.Code)
	}

	// Now the ban is in force. A page path answers text/plain...
	w := do(t, r, "GET", "/ops1", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a banned source on a page path = %d, want 403", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("a banned page path came back as %q, want text/plain", ct)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 403 from the ban carried no Retry-After")
	}
	// ...and an API path answers in the envelope.
	w = do(t, r, "GET", "/api/clips/ops1", nil, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a banned source on an API path = %d, want 403", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("a banned API path came back as %q, want the JSON envelope", ct)
	}

	// A different source is untouched: the ban is per address.
	if w := doFrom(t, r, "203.0.113.9", "GET", "/ops1", ""); w.Code != http.StatusOK {
		t.Errorf("a fresh source = %d, want 200; the ban is not per address", w.Code)
	}
}

// TestATrustedPeerIsNeverBanned is the single most dangerous thing this layer
// could get wrong. The service's immediate peer is the local tunnel client, so a
// request whose forwarding header did not arrive keys as loopback; banning it
// would refuse every client on the internet at once.
func TestATrustedPeerIsNeverBanned(t *testing.T) {
	r := testServer(t, func(c *Config) {
		c.RequestPerMinute = 1
		c.BanAfter = 1
		c.BanFor = time.Hour
		c.BanMaxFor = time.Hour
	})

	for i := 0; i < 10; i++ {
		w := doFrom(t, r, "127.0.0.1", "GET", "/ops1", "")
		if w.Code == http.StatusForbidden {
			t.Fatalf("request %d from a trusted peer was banned; this refuses the whole internet", i+1)
		}
	}
}

// TestAuthFailuresAloneDoNotBan is the other half of the charging rule: only a
// 429 charges a strike. A person who opens a locked clip without its password,
// or mistypes it, must not drift toward a ban.
func TestAuthFailuresAloneDoNotBan(t *testing.T) {
	r := testServer(t, func(c *Config) {
		c.UnlockPerHour = 1000 // generous, so no 429 is produced
		c.BanAfter = 2
		c.BanFor = time.Hour
		c.BanMaxFor = time.Hour
	})
	if w := do(t, r, "PUT", "/api/clips/ops1",
		map[string]any{"body": "x", "password": "s3cret"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}

	for i := 0; i < 10; i++ {
		// /raw is the gated read path. The page path answers 200 with a locked
		// shell instead (the body is what is withheld there), so a 401 has to be
		// asked for where this service actually emits one.
		w := do(t, r, "GET", "/ops1/raw", nil, map[string]string{"X-Clip-Password": "wrong"})
		if w.Code == http.StatusForbidden {
			t.Fatalf("wrong-password attempt %d was banned; a 401 must never charge a strike", i+1)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong-password attempt %d = %d, want 401", i+1, w.Code)
		}
	}
}
