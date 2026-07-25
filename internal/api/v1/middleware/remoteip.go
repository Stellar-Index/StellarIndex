package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

var trustedProxyConfig struct {
	sync.RWMutex
	prefixes []netip.Prefix
}

// SetTrustedProxyCIDRs replaces the allow-list used to decide whether
// X-Forwarded-For should influence client identity. Empty means "trust
// no proxies" and therefore ignore XFF entirely.
func SetTrustedProxyCIDRs(cidrs []string) error {
	parsed := make([]netip.Prefix, 0, len(cidrs))
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, prefix.Masked())
	}

	trustedProxyConfig.Lock()
	trustedProxyConfig.prefixes = parsed
	trustedProxyConfig.Unlock()
	return nil
}

func remoteIPFor(r *http.Request) string {
	peer := remoteAddrHost(r.RemoteAddr)
	if peer == "" {
		return ""
	}
	if !requestCameViaTrustedProxy(peer) {
		return peer
	}
	if client := rightmostUntrustedForwardedFor(r.Header.Get("X-Forwarded-For")); client != "" {
		return client
	}
	return peer
}

func resolveRemoteIP(r *http.Request) string {
	return remoteIPFor(r)
}

// remoteIPPrefixFor resolves the client's THROTTLE-KEY identity: the
// exact address for IPv4, or the /64 network prefix for IPv6.
//
// SECURITY (SEC-15): keying a per-IP throttle on the full IPv6 /128
// address is not equivalent to keying an IPv4 throttle on its /32 —
// residential and mobile ISPs commonly delegate an entire /64 (or
// larger) to a single subscriber, so an attacker with any routable
// IPv6 prefix can mint an unbounded number of distinct /128 addresses
// and get a fresh throttle bucket for each one, bypassing the limit
// entirely. Aggregating to /64 — the smallest block a residential ISP
// typically delegates per RFC 7421 — makes the throttle key match the
// unit an attacker actually controls cheaply. IPv4 has no equivalent
// cheap-rotation problem at the scale attackers actually have access
// to, so it keeps its full address.
//
// This must be used ONLY for throttle/rate-limit/cap keys — never for
// audit logging, admin display, or anything that wants the caller's
// precise address. [remoteIPFor] / [RemoteIP] remain the exact
// resolver for those.
func remoteIPPrefixFor(r *http.Request) string {
	return maskIPForThrottleKey(remoteIPFor(r))
}

// maskIPForThrottleKey masks a resolved IP string down to its
// throttle-key identity per [remoteIPPrefixFor]'s policy: unchanged
// for IPv4, the /64 network prefix for IPv6. Returns ip unchanged
// (including "") if it doesn't parse as an address.
func maskIPForThrottleKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr.Is4() || addr.Is4In6() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().Addr().String()
}

// RemoteIP returns the request's caller IP, honouring trusted-proxy
// CIDRs the operator configured via [SetTrustedProxyCIDRs]. When
// the request arrived via a trusted proxy, `X-Forwarded-For` is
// walked RIGHT-TO-LEFT and the rightmost entry NOT contained in any
// trusted-proxy CIDR is returned (the closest untrusted hop);
// otherwise the direct peer.
//
// Exported for handlers that need to derive a per-IP throttle key
// outside the middleware chain — e.g. `/v1/signup` per-IP signup
// cap (F-1232 audit-2026-05-12). Returns "" when no IP can be
// resolved (well-formed requests always have one; the empty case
// is left to the caller's policy).
func RemoteIP(r *http.Request) string {
	return remoteIPFor(r)
}

func requestCameViaTrustedProxy(peer string) bool {
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}

	trustedProxyConfig.RLock()
	defer trustedProxyConfig.RUnlock()
	for _, prefix := range trustedProxyConfig.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// rightmostUntrustedForwardedFor walks the X-Forwarded-For chain
// RIGHT-TO-LEFT and returns the first entry NOT contained in any
// trusted-proxy CIDR — i.e. the closest untrusted hop to us.
//
// This is the only XFF parse that is safe against forging. Most L7
// proxies (nginx's `proxy_add_x_forwarded_for`, AWS ALB, Cloudflare,
// Caddy, …) APPEND the immediate peer to whatever XFF the client
// sent rather than replacing it, so the chain looks like:
//
//	<client-supplied...>, <trusted-proxy-1>, <trusted-proxy-2>
//
// Taking the LEFTMOST entry would let a client forge its own value
// (`X-Forwarded-For: 1.2.3.4` from the client survives at the head
// of the chain), spoofing per-IP rate-limit identity and bypassing
// per-key IP allowlists. Walking right-to-left, every trailing entry
// that falls inside a trusted CIDR is a hop we control; the first
// entry that does NOT is the real caller (or the furthest-out hop we
// can still attribute). Anything to the LEFT of it is attacker-
// controlled and ignored.
//
// Returns "" when the header is empty, every parseable entry is
// trusted (degenerate — caller falls back to the direct peer), or
// no entry parses as an IP.
func rightmostUntrustedForwardedFor(xff string) string {
	if xff == "" {
		return ""
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		entry := strings.TrimSpace(parts[i])
		if entry == "" {
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			// A malformed hop breaks the trust chain: we can no longer
			// be sure entries to its left are attacker-controlled vs
			// trusted, so stop and let the caller fall back to the
			// direct peer rather than trusting a guessed value.
			return ""
		}
		if requestCameViaTrustedProxy(addr.String()) {
			continue
		}
		return addr.String()
	}
	return ""
}

func remoteAddrHost(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return addr.String()
	}
	return remoteAddr
}
