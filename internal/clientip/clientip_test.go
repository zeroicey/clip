package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func clientIPRequest(remote, xff, cf string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/clips/ops1", nil)
	r.RemoteAddr = remote
	if xff != "" {
		for _, v := range strings.Split(xff, "|") {
			r.Header.Add("X-Forwarded-For", v)
		}
	}
	if cf != "" {
		r.Header.Set("Cf-Connecting-Ip", cf)
	}
	return r
}

// TestIPSpoofingIsIgnored is the regression test for a rate-limit bypass: a
// direct (untrusted) peer must not be able to influence its own bucket with any
// forwarded header.
func TestIPSpoofingIsIgnored(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		cf     string
		want   string
	}{
		{"peer only", "1.2.3.4:5", "", "", "1.2.3.4"},
		{"forged xff ignored", "1.2.3.4:5", "9.9.9.9", "", "1.2.3.4"},
		{"forged cf ignored", "1.2.3.4:5", "", "9.9.9.9", "1.2.3.4"},
		{"both forged ignored", "1.2.3.4:5", "9.9.9.9", "9.9.9.9", "1.2.3.4"},
		{"forged xff chain ignored", "1.2.3.4:5", "8.8.8.8, 9.9.9.9", "8.8.4.4", "1.2.3.4"},
		{"untrusted public peer", "203.0.113.7:443", "10.0.0.5", "", "203.0.113.7"},
		{"untrusted v6 peer", "[2001:db8::5]:99", "9.9.9.9", "", "2001:db8::5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(clientIPRequest(tc.remote, tc.xff, tc.cf)); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrustedProxyWalksRightToLeft covers the chain a real deployment produces:
// the peer is the local frpc on loopback, and Caddy's `header_up
// X-Forwarded-For {remote_host}` puts the real client in the header.
func TestTrustedProxyWalksRightToLeft(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"single hop through frpc", "127.0.0.1:5", "9.9.9.9", "9.9.9.9"},
		{"caddy append: rightmost untrusted", "127.0.0.1:5", "8.8.8.8, 9.9.9.9", "9.9.9.9"},
		{"spoofed leftmost loses to the append", "127.0.0.1:5", "1.2.3.4, 9.9.9.9", "9.9.9.9"},
		{"multi-hop skips trusted", "127.0.0.1:5", "8.8.8.8, 172.17.0.1", "8.8.8.8"},
		{"all hops trusted falls back to peer", "127.0.0.1:5", "10.0.0.1, 192.168.1.1", "127.0.0.1"},
		{"private peer", "192.168.1.9:5", "8.8.8.8", "8.8.8.8"},
		{"ula peer v6", "[fd00::1]:5", "8.8.8.8", "8.8.8.8"},
		{"loopback v6 peer", "[::1]:5", "2001:db8::1", "2001:db8::1"},
		{"mapped v4 peer is unmapped", "[::ffff:127.0.0.1]:5", "8.8.8.8", "8.8.8.8"},
		{"no header uses peer", "127.0.0.1:5", "", "127.0.0.1"},
		{"bare host peer", "127.0.0.1", "8.8.8.8", "8.8.8.8"},
		{"bracketed v6 peer without port", "[fd00::1]", "8.8.8.8", "8.8.8.8"},
		{"multiple header lines in order", "127.0.0.1:5", "8.8.8.8|9.9.9.9", "9.9.9.9"},
		{"leading/trailing spaces", "127.0.0.1:5", "  8.8.8.8 ,  9.9.9.9  ", "9.9.9.9"},
		{"empty header lines", "127.0.0.1:5", " | ", "127.0.0.1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(clientIPRequest(tc.remote, tc.xff, "")); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRejectsNonIPEntries ensures a malformed forwarded value can never become a
// bucket, and that junk on RemoteAddr cannot panic.
func TestRejectsNonIPEntries(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"junk entry skipped", "127.0.0.1:5", "not-an-ip, 9.9.9.9", "9.9.9.9"},
		{"all junk falls back to peer", "127.0.0.1:5", "not-an-ip, host:99, ", "127.0.0.1"},
		{"cidr entry skipped", "127.0.0.1:5", "8.8.8.0/24, 9.9.9.9", "9.9.9.9"},
		{"named host with port skipped", "127.0.0.1:5", "proxy.internal:80, 9.9.9.9", "9.9.9.9"},
		{"v4 with port on remote", "1.2.3.4:5", "", "1.2.3.4"},
		{"garbage remote passes through", "somewhere", "8.8.8.8", "somewhere"},
		{"empty remote passes through", "", "8.8.8.8", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClientIP(clientIPRequest(tc.remote, tc.xff, ""))
			if got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
			// Whatever comes back must either be the unparseable peer itself or a
			// real IP literal — never a forwarded string smuggled through.
			if _, err := netip.ParseAddr(got); err != nil && got != tc.remote {
				t.Errorf("ClientIP() = %q is neither an IP nor the raw peer %q", got, tc.remote)
			}
		})
	}
}

// TestReadsCaddyRemoteFormat is the regression test for a production incident:
// Caddy's `{remote}` placeholder expands to `host:port`, not a bare address, and
// treating those entries as junk discarded the only real client address in the
// chain. Every client then fell back to the proxy's own address, so three
// unrelated clients shared one hourly quota.
//
// The header values below are copied verbatim from what Caddy actually emits. Do
// NOT "simplify" them to bare IPs: doing so is exactly how the original test
// missed the bug, because the simulation of the proxy used a format the proxy
// never produces.
func TestReadsCaddyRemoteFormat(t *testing.T) {
	cases := []struct {
		name string
		xff  string
		want string
	}{
		{"caddy remote v4", "121.9.113.26:54321", "121.9.113.26"},
		{"caddy remote v6", "[2001:db8::1]:54321", "2001:db8::1"},
		{"forged prefix plus real client with port", "6.6.6.6, 121.9.113.26:54321", "121.9.113.26"},
		{"trusted rightmost, ported entry to its left", "1.2.3.4:5678, 172.17.0.5", "1.2.3.4"},
		{"bracketed v6 without port", "[2001:db8::1]", "2001:db8::1"},
		{"bare literal still works", "9.9.9.9", "9.9.9.9"},
		{"junk still falls back to the peer", "unknown, _hidden, 10.0.0.0/8", "127.0.0.1"},
		{"named host with port still falls back", "proxy.internal:80", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(clientIPRequest("127.0.0.1:41234", tc.xff, "")); got != tc.want {
				t.Errorf("ClientIP(xff=%q) = %q, want %q", tc.xff, got, tc.want)
			}
		})
	}

	// The production symptom itself: unrelated clients must not collapse into one
	// bucket.
	t.Run("distinct clients get distinct buckets", func(t *testing.T) {
		seen := map[string]bool{}
		for _, remote := range []string{
			"121.9.113.26:54321",   // Guangdong Telecom
			"124.221.144.97:54321", // Tencent Cloud
			"47.107.1.2:54321",     // Aliyun Guangzhou
		} {
			got := ClientIP(clientIPRequest("127.0.0.1:41234", remote, ""))
			if got == "127.0.0.1" {
				t.Errorf("ClientIP(xff=%q) collapsed to the proxy peer", remote)
			}
			if seen[got] {
				t.Errorf("ClientIP(xff=%q) = %q, already used by another client", remote, got)
			}
			seen[got] = true
		}
		if len(seen) != 3 {
			t.Errorf("got %d buckets for 3 clients, want 3", len(seen))
		}
	})
}

// TestCloudflareHeaderIsAlwaysIgnored pins the deliberate absence of the
// TrustCloudflare escape hatch the teleport service carries. If someone adds the
// flag back, this test is what makes them argue for it first.
func TestCloudflareHeaderIsAlwaysIgnored(t *testing.T) {
	// Trusted peer: only the XFF walk may decide.
	if got := ClientIP(clientIPRequest("127.0.0.1:5", "8.8.8.8", "9.9.9.9")); got != "8.8.8.8" {
		t.Errorf("ClientIP() = %q, want 8.8.8.8 from XFF, ignoring Cf-Connecting-Ip", got)
	}
	if got := ClientIP(clientIPRequest("127.0.0.1:5", "", "9.9.9.9")); got != "127.0.0.1" {
		t.Errorf("ClientIP() = %q, want the peer; Cf-Connecting-Ip must not be honoured", got)
	}
	// Untrusted peer gains nothing from it either.
	if got := ClientIP(clientIPRequest("1.2.3.4:5", "", "9.9.9.9")); got != "1.2.3.4" {
		t.Errorf("ClientIP() = %q, want 1.2.3.4", got)
	}
	// A malformed XFF still falls back to the peer rather than to the CF header.
	if got := ClientIP(clientIPRequest("127.0.0.1:5", "not-an-ip", "9.9.9.9")); got != "127.0.0.1" {
		t.Errorf("ClientIP() = %q, want 127.0.0.1", got)
	}
}

// TestIsTrustedIsTheBanGuard pins the addresses that must never be banned.
//
// The service's immediate peer is the local tunnel client, so a request whose
// forwarding header did not arrive keys as loopback. Banning it would refuse the
// entire internet at once — this is the single most dangerous thing a ban list
// can do here, which is why the property is asserted rather than assumed.
func TestIsTrustedIsTheBanGuard(t *testing.T) {
	trusted := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.17.0.1", "192.168.1.1", "fc00::1",
		// Unparseable values fail closed (treated as trusted = never bannable).
		"", "not-an-ip", "1.2.3.4/24", "unknown",
	}
	for _, ip := range trusted {
		if !IsTrusted(ip) {
			t.Errorf("IsTrusted(%q) = false; a ban could target a shared or unattributable address", ip)
		}
	}
	untrusted := []string{
		"203.0.113.9", "198.51.100.7", "8.8.8.8", "2001:db8::1",
		// A port-bearing form is still a real client address, not a proxy.
		"203.0.113.9:40000",
	}
	for _, ip := range untrusted {
		if IsTrusted(ip) {
			t.Errorf("IsTrusted(%q) = true; a real client would be exempt from banning", ip)
		}
	}
}
