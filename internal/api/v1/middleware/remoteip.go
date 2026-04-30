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
	if client := firstForwardedFor(r.Header.Get("X-Forwarded-For")); client != "" {
		return client
	}
	return peer
}

func resolveRemoteIP(r *http.Request) string {
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

func firstForwardedFor(xff string) string {
	if xff == "" {
		return ""
	}
	first := xff
	if comma := strings.IndexByte(xff, ','); comma >= 0 {
		first = xff[:comma]
	}
	first = strings.TrimSpace(first)
	addr, err := netip.ParseAddr(first)
	if err != nil {
		return ""
	}
	return addr.String()
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
