// Package clientip decides which address a request should be attributed to for
// security decisions: rate limiting, audit, attribution.
//
// # Why this is its own package
//
// It was extracted from the teleport service, where it was written against a
// production incident: Caddy's `{remote}` placeholder expands to `host:port`,
// and an earlier version treated that as junk — so every client behind the proxy
// collapsed into the proxy's own address and shared one rate-limit bucket. The
// parsing rules below (and their tests) are the fix for that, and they are the
// same rules regardless of which service is asking.
//
// It is deliberately a *copy* rather than a shared module. Relay's two services
// live in separate repositories on purpose, and a one-file package used by two
// independently deployed programs is not worth a dependency between them.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// trustedProxyPrefixes are the immediate peers whose forwarded headers are
// believed.
//
// Loopback is the important one here: the clip service listens on 127.0.0.1 only
// and is reached through an frp tunnel, so the immediate peer is always the local
// frpc. Caddy on hcyj sets X-Forwarded-For to the real client, which makes the
// whole chain trusted-by-construction: the only way to reach this process is
// through that proxy.
//
// Parsed once at init, never per request.
var trustedProxyPrefixes = mustParsePrefixes(
	"127.0.0.0/8",    // IPv4 loopback
	"::1/128",        // IPv6 loopback
	"10.0.0.0/8",     // RFC 1918
	"172.16.0.0/12",  // RFC 1918 — docker bridges live here
	"192.168.0.0/16", // RFC 1918
	"fc00::/7",       // IPv6 unique local addresses
)

// ClientIP returns the client address for security decisions.
//
// It is the *exact inverse* of a naive "RealIP" helper: it trusts a forwarded
// header only when the immediate peer is a known proxy, and it reads
// X-Forwarded-For from the right, because that is the end a trusted proxy
// appended to.
//
//  1. The peer is RemoteAddr with the port stripped.
//  2. An untrusted peer returns the peer itself and every header is ignored. A
//     forged `X-Forwarded-For` therefore has no effect at all on a direct
//     connection — this is the property that makes the return value usable as a
//     security boundary.
//  3. A trusted peer walks X-Forwarded-For right to left, skipping proxies in
//     trustedProxyPrefixes, and returns the first untrusted address. Entries
//     that name no address at all (bare junk, CIDRs, names like "unknown") are
//     skipped rather than guessed at, so a non-IP string can never become a
//     bucket. An entry that names an address unambiguously is read even when it
//     carries a port — see parseIP.
//  4. `Cf-Connecting-Ip` is ignored, always. There is deliberately no flag to
//     change that (see below).
//
// If nothing usable is found the peer is returned, so every unparseable case
// collapses into one shared bucket rather than a fresh one per request.
//
// Honest limits: this makes IP attribution *unspoofable for a direct
// connection*; it does not make IP-keyed rate limiting a defence. An attacker
// with a handful of real addresses — or a single IPv6 /64, which contains 2^64
// addresses — gets a fresh bucket per source address no matter how correct this
// parsing is. Per-IP limits are a speed bump; the real backstop for a public
// write endpoint is the global quota in the store.
func ClientIP(r *http.Request) string {
	peer := peerHost(r.RemoteAddr)
	peerAddr, ok := parseIP(peer)
	if !ok {
		// RemoteAddr is not an IP at all. It comes from the connection, not from
		// a header, so we pass it through unchanged: callers get one shared
		// "unparseable" bucket, which fails closed rather than fresh.
		return peer
	}
	if !isTrustedProxy(peerAddr) {
		// Direct connection: all forwarded headers are attacker-controlled.
		return peerAddr.String()
	}

	// Right to left: the rightmost entry is the one the closest trusted proxy
	// appended, so it is the last value an attacker could not have chosen.
	// Anything further left may be forged, which is why the walk stops at the
	// first untrusted address instead of trusting the leftmost one.
	parts := forwardedFor(r)
	for i := len(parts) - 1; i >= 0; i-- {
		addr, ok := parseIP(parts[i])
		if !ok || isTrustedProxy(addr) {
			continue
		}
		return addr.String()
	}
	return peerAddr.String()
}

// Note on `Cf-Connecting-Ip`: the teleport service has a TrustCloudflare flag for
// it, defaulted off, with a long comment about how a half-fronted deployment is
// worse than none. This service does not carry the flag at all. Nothing in the
// path to clip.hcyj.xyz terminates at a CDN (DNS points straight at the hcyj box,
// and the request arrives through an frp tunnel), so the header is purely
// client-chosen; a switch that can only ever be flipped wrongly is better
// deleted than documented. Removing the option is the guard.

// forwardedFor flattens every X-Forwarded-For header, in received order.
func forwardedFor(r *http.Request) []string {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return nil
	}
	var out []string
	for _, v := range values {
		out = append(out, strings.Split(v, ",")...)
	}
	return out
}

// peerHost strips the port from RemoteAddr, tolerating a bare host.
func peerHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.Trim(remoteAddr, "[]")
}

// parseIP parses a single address token from a forwarded header or from
// RemoteAddr.
//
// Accepted: a bare literal ("1.2.3.4", "2001:db8::1"), a bracketed literal
// ("[2001:db8::1]"), and either of those with a port ("1.2.3.4:5678",
// "[2001:db8::1]:5678").
//
// The port forms are read, not guessed at, because they still name exactly one
// host. This is not hypothetical: Caddy's `{remote}` placeholder expands to
// `host:port`, so refusing to read it discarded the only real client address in
// the chain and fell back to the proxy's own address — every client in the world
// shared one rate-limit bucket. The regression test uses Caddy's literal output
// format for that reason.
//
// Rejected, rather than guessed at: CIDRs, host names ("unknown", "_hidden",
// "host:99"), empty and malformed tokens. A value that names no address must
// never become a rate-limit bucket.
//
// IPv4-mapped IPv6 forms are unmapped so one host cannot be split into two
// buckets, and any zone is dropped so a scoped literal keys like an unscoped one.
func parseIP(s string) (netip.Addr, bool) {
	token := strings.TrimSpace(s)
	if addr, err := netip.ParseAddr(token); err == nil {
		return addr.Unmap().WithZone(""), true
	}
	// "host:port" and "[v6]:port".
	if addrPort, err := netip.ParseAddrPort(token); err == nil {
		return addrPort.Addr().Unmap().WithZone(""), true
	}
	// A bracketed literal with no port: "[2001:db8::1]".
	if len(token) > 1 && token[0] == '[' && token[len(token)-1] == ']' {
		if addr, err := netip.ParseAddr(token[1 : len(token)-1]); err == nil {
			return addr.Unmap().WithZone(""), true
		}
	}
	return netip.Addr{}, false
}

// isTrustedProxy reports whether addr is one of the proxies whose forwarded
// headers we believe.
func isTrustedProxy(addr netip.Addr) bool {
	for _, prefix := range trustedProxyPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func mustParsePrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			// Constant input: a typo here is a programming error, and failing at
			// init is better than silently trusting nothing (or everything).
			panic("clientip: invalid trusted proxy CIDR " + c + ": " + err.Error())
		}
		out = append(out, prefix.Masked())
	}
	return out
}
