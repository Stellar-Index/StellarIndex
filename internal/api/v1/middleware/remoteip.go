package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

var trustedProxyConfig struct {
	sync.RWMutex
	prefixes []netip.Prefix
}

// SetTrustedProxyCIDRs replaces the allow-list deciding whether X-Forwarded-For influences client identity; empty trusts no proxies and
// ignores XFF entirely.
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

// remoteIPPrefixFor resolves the THROTTLE-KEY identity via [ratelimit.ThrottleIPKey]: unmapped IPv4 ("::ffff:1.2.3.4" = "1.2.3.4") or
// the IPv6 /64. ONLY for throttle and cap keys; audit logs and admin display need the exact address from [remoteIPFor] / [RemoteIP].
func remoteIPPrefixFor(r *http.Request) string {
	return ratelimit.ThrottleIPKey(remoteIPFor(r))
}

// RemoteIP returns the caller IP: behind a trusted proxy ([SetTrustedProxyCIDRs]) the closest untrusted X-Forwarded-For hop, else the
// direct peer; "" when none resolves.
func RemoteIP(r *http.Request) string {
	return remoteIPFor(r)
}

// RemoteIPThrottleKey is the exported form of [remoteIPPrefixFor]: the /64-masked throttle identity for handler-level per-IP caps.
func RemoteIPThrottleKey(r *http.Request) string {
	return remoteIPPrefixFor(r)
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

// rightmostUntrustedForwardedFor walks X-Forwarded-For RIGHT-TO-LEFT and returns the first entry outside every trusted-proxy CIDR. The
// leftmost entry is attacker-controlled. Returns "" if empty, all trusted (caller uses the direct peer) or none parses as an IP.
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
			// A malformed hop breaks the trust chain: we cannot
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
