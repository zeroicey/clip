package clip

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zeroicey/clip/internal/clientip"
)

// This file is the request-level defence layer: the headers every answer
// carries, the per-source request budget, the ban that a spent budget escalates
// into, and the body ceiling that holds for routes that forget one.
//
// It sits in front of the handlers on purpose. The handler-level throttles
// (writes, secret guesses) know *what* is being attempted and stay where they
// are; this layer knows only that a request arrived, and it exists so that a
// request which is refused never reaches the database, the argon2 verifier or
// the long-poll hub at all.
//
// Two properties are load-bearing and easy to lose in a refactor:
//
//   - The key is clientip.ClientIP, never a raw forwarding header. A header a
//     client can set would give every request its own bucket (see
//     .ai/pitfalls/cases/2026-10-10-trusted-request-header-grants-authority.md).
//   - A trusted peer is never banned (clientip.IsTrusted). The service's
//     immediate peer is the local tunnel client, so a request without a
//     forwarding header keys as loopback — banning it would refuse the whole
//     internet. A ban list is the one defence whose failure mode is total.

// securityHeaders applies the baseline every response should carry, including
// the ones a handler returns early from (a 400, a 404, a 429, a ban).
//
// It is a middleware rather than a line in each handler because the handlers
// that forget are exactly the error paths, and a security header that is missing
// on the error path is missing where it matters most. Handlers that need a
// different value replace it: the HTML pages install a per-response nonce policy
// and /raw installs a sandbox one, both of which overwrite the baseline set here.
//
// HSTS is deliberately NOT set here. It belongs to the TLS layer, and the
// reverse proxy in front of this service already sends it (with a longer max-age
// and `preload`). Setting it here too produced two Strict-Transport-Security
// headers on every live response — a browser processes only the first, so the
// duplicate was harmless in effect and still a contract lie. Two headers for one
// policy is the same failure family as the duplicate CSP recorded in
// .ai/pitfalls/cases/2026-10-18-duplicate-security-header-with-the-proxy.md.
func (s *Server) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		// Note what is NOT denied: clipboard-write. The page's copy button calls
		// navigator.clipboard.writeText, and `clipboard-write=()` would switch it
		// off — a security header that breaks the product's main action.
		h.Set("Permissions-Policy",
			"accelerometer=(), autoplay=(), camera=(), geolocation=(), gyroscope=(), "+
				"magnetometer=(), microphone=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// A baseline for the answers that do not set their own: the JSON API, the
		// plain-text errors, robots.txt, the favicon. The two HTML page renderers
		// and /raw replace it with a policy tailored to their content.
		h.Set("Content-Security-Policy",
			"default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		c.Next()
	}
}

// guard is the ban gate, the per-source request budget, and the escalation
// between them.
//
// The escalation is deliberately one-directional and cheap to reason about: a
// request is refused for exceeding a budget (429), and that refusal is what
// feeds the ban. Nothing else charges a strike — in particular a 401 does not,
// because opening a locked clip without its password is an ordinary page load,
// not an attack, and a person who mistypes a password must not be a few mistakes
// away from losing access to the whole service.
func (s *Server) guard() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := clientip.ClientIP(c.Request)
		trusted := clientip.IsTrusted(ip)

		// A ban is checked before anything else, so a banned caller costs this
		// process a map lookup and nothing more.
		if !trusted {
			if banned, wait := s.bans.Blocked(ip); banned {
				c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
				refuse(c, http.StatusForbidden, "banned", "这个来源地址已被暂时拒绝访问，请稍后再试。")
				return
			}
		}

		// The global budget. It covers reads as well as writes: the write budget
		// is a separate, much tighter bucket inside the handlers, but an
		// unauthenticated read still costs a database round trip, and before this
		// existed a single address could drive the read paths without limit.
		if !s.requestLimiter.Allow(ip) {
			c.Header("Retry-After", strconv.Itoa(s.requestLimiter.RetryAfter()))
			refuse(c, http.StatusTooManyRequests, "rate_limited", "请求太频繁了，稍后再试。")
			s.chargeStrike(ip, trusted, "request_budget")
			return
		}

		c.Next()

		// Every 429 this service emits comes from one of its own budgets, so it is
		// exactly the "you were told to stop" signal a ban should escalate from.
		if c.Writer.Status() == http.StatusTooManyRequests {
			s.chargeStrike(ip, trusted, "refused")
		}
	}
}

// chargeStrike records one refusal against ip and logs the ban when it lands.
//
// The ban event is logged once, here, rather than on every blocked request:
// logging each refusal of an already-banned address is how a ban list turns into
// a log flood. The refusals themselves are still visible in the access log.
func (s *Server) chargeStrike(ip string, trusted bool, reason string) {
	if trusted {
		// Never ban a shared or unattributable address (see clientip.IsTrusted).
		return
	}
	if banned, duration, strikes := s.bans.Strike(ip); banned {
		s.log.Warn("banned address",
			"ip", ip,
			"reason", reason,
			"duration", duration.String(),
			"strikes", strikes,
		)
	}
}

// bodyLimit caps the bytes any request may carry, before a handler parses them.
//
// The handlers each apply their own, tighter limit (the JSON text limit, the
// multipart asset total); this is the backstop that still holds if a route is
// added later without one, and it bounds the read for the public-path handlers,
// which never look at a body at all.
func (s *Server) bodyLimit() gin.HandlerFunc {
	// The largest thing this service ever legitimately accepts: a full set of
	// images plus the JSON text limit plus framing slack.
	maxTotal := s.cfg.MaxAssetBytes*int64(s.cfg.MaxAssets) + s.cfg.MaxBodyBytes + 1<<20
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTotal)
		}
		c.Next()
	}
}

// refuse answers a request the middleware layer rejected, in the shape its
// namespace promises: the JSON envelope under /api, text/plain everywhere else.
//
// Getting this wrong is silent — a page-shaped body on an API path looks like a
// proxy error to a client, which is the contract break this service already hit
// once on the live deployment (see
// .ai/pitfalls/cases/api-path-that-forgot-the-envelope.md).
func refuse(c *gin.Context, status int, code, message string) {
	if path := c.Request.URL.Path; path == "/api" || strings.HasPrefix(path, "/api/") {
		fail(c, status, code, message)
		return
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(status, message+"\n")
	c.Abort()
}

// retryAfterSeconds rounds a remaining duration up to whole seconds, with a
// floor of one so a Retry-After is never "0".
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}
